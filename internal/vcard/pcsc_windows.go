//go:build windows

package vcard

// Windows PC/SC backend. Unlike the macOS and Linux backends this needs no cgo:
// the Smart Card API lives in winscard.dll, which we bind through
// golang.org/x/sys/windows. That keeps Windows builds fully static and
// cross-compilable from any host.
//
// The API mirrors the PC/SC Workgroup spec that pcsclite and macOS also
// implement, so the flow here is the same as the other backends: establish a
// context, list readers, connect to the first one, then transmit APDUs.

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winscard = windows.NewLazySystemDLL("winscard.dll")

	procEstablishContext = winscard.NewProc("SCardEstablishContext")
	procReleaseContext   = winscard.NewProc("SCardReleaseContext")
	procListReaders      = winscard.NewProc("SCardListReadersW")
	procConnect          = winscard.NewProc("SCardConnectW")
	procDisconnect       = winscard.NewProc("SCardDisconnect")
	procStatus           = winscard.NewProc("SCardStatusW")
	procTransmit         = winscard.NewProc("SCardTransmit")
)

// PC/SC constants (winsmcrd.h / winscard.h).
const (
	scardScopeSystem = 2

	scardShareShared = 2

	scardProtocolT0  = 1
	scardProtocolT1  = 2
	scardProtocolAny = scardProtocolT0 | scardProtocolT1

	scardLeaveCard = 0

	scardStatePresent = 0x0020

	scardSuccess = 0

	// Returned by SCardConnect when the reader is empty; not a hard failure.
	scardENoSmartcard = 0x8010000C
	scardWRemovedCard = 0x80100069

	maxATRSize = 33
)

// scardIORequest matches the SCARD_IO_REQUEST struct: protocol + struct length.
type scardIORequest struct {
	Protocol uint32
	PciLen   uint32
}

// WinPCSCReader is the Windows PC/SC implementation of Reader.
type WinPCSCReader struct {
	ctx   uintptr
	card  uintptr
	proto uint32
	name  string
	atr   []byte
}

// scardErr turns a non-zero SCARD return code into an error.
func scardErr(what string, rv uintptr) error {
	return fmt.Errorf("%s: 0x%08x", what, uint32(rv))
}

// NewWinPCSCReader establishes a PC/SC context and connects to the first
// reader. A reader with no card inserted is not an error — Present() reports
// that separately, matching the other backends.
func NewWinPCSCReader() (*WinPCSCReader, error) {
	r := &WinPCSCReader{}

	rv, _, _ := procEstablishContext.Call(
		uintptr(scardScopeSystem), 0, 0, uintptr(unsafe.Pointer(&r.ctx)))
	if rv != scardSuccess {
		return nil, scardErr("SCardEstablishContext", rv)
	}

	// Ask for the buffer size first, then fetch the multi-string of reader
	// names (UTF-16, each NUL-terminated, the list ending in a double NUL).
	var n uint32
	rv, _, _ = procListReaders.Call(r.ctx, 0, 0, uintptr(unsafe.Pointer(&n)))
	if rv != scardSuccess || n == 0 {
		procReleaseContext.Call(r.ctx)
		if rv != scardSuccess {
			return nil, scardErr("SCardListReaders", rv)
		}
		return nil, fmt.Errorf("no smart-card readers attached")
	}
	buf := make([]uint16, n)
	rv, _, _ = procListReaders.Call(r.ctx, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if rv != scardSuccess {
		procReleaseContext.Call(r.ctx)
		return nil, scardErr("SCardListReaders", rv)
	}
	r.name = firstMultiString(buf)
	if r.name == "" {
		procReleaseContext.Call(r.ctx)
		return nil, fmt.Errorf("no smart-card readers attached")
	}

	namePtr, err := windows.UTF16PtrFromString(r.name)
	if err != nil {
		procReleaseContext.Call(r.ctx)
		return nil, err
	}
	var activeProto uint32
	rv, _, _ = procConnect.Call(r.ctx, uintptr(unsafe.Pointer(namePtr)),
		uintptr(scardShareShared), uintptr(scardProtocolAny),
		uintptr(unsafe.Pointer(&r.card)), uintptr(unsafe.Pointer(&activeProto)))
	if rv != scardSuccess {
		if uint32(rv) == scardENoSmartcard || uint32(rv) == scardWRemovedCard {
			// Reader present, no card. Keep the context so Status() can say so.
			return r, nil
		}
		procReleaseContext.Call(r.ctx)
		return nil, scardErr("SCardConnect", rv)
	}
	r.proto = activeProto
	r.readATR()
	return r, nil
}

// readATR fetches the Answer-To-Reset of the connected card, best effort.
func (r *WinPCSCReader) readATR() {
	if r.card == 0 {
		return
	}
	atr := make([]byte, maxATRSize)
	atrLen := uint32(len(atr))
	var state, proto uint32
	var nameLen uint32
	rv, _, _ := procStatus.Call(r.card, 0, uintptr(unsafe.Pointer(&nameLen)),
		uintptr(unsafe.Pointer(&state)), uintptr(unsafe.Pointer(&proto)),
		uintptr(unsafe.Pointer(&atr[0])), uintptr(unsafe.Pointer(&atrLen)))
	if rv == scardSuccess && atrLen <= uint32(len(atr)) {
		r.atr = atr[:atrLen]
	}
}

// Transmit sends an APDU to the card and returns the response.
func (r *WinPCSCReader) Transmit(apdu []byte) ([]byte, error) {
	if r.card == 0 {
		return nil, fmt.Errorf("no card connected")
	}
	if len(apdu) == 0 {
		return nil, fmt.Errorf("empty APDU")
	}
	proto := r.proto
	if proto == 0 {
		proto = scardProtocolT1
	}
	sendPci := scardIORequest{Protocol: proto, PciLen: uint32(unsafe.Sizeof(scardIORequest{}))}

	resp := make([]byte, 258) // max short-APDU response + SW1/SW2
	respLen := uint32(len(resp))
	rv, _, _ := procTransmit.Call(r.card,
		uintptr(unsafe.Pointer(&sendPci)),
		uintptr(unsafe.Pointer(&apdu[0])), uintptr(len(apdu)),
		0,
		uintptr(unsafe.Pointer(&resp[0])), uintptr(unsafe.Pointer(&respLen)))
	if rv != scardSuccess {
		return nil, scardErr("SCardTransmit", rv)
	}
	if respLen > uint32(len(resp)) {
		respLen = uint32(len(resp))
	}
	return resp[:respLen], nil
}

// Present reports whether a card is currently inserted.
func (r *WinPCSCReader) Present() bool {
	if r.card == 0 {
		return false
	}
	var state, proto, nameLen, atrLen uint32
	rv, _, _ := procStatus.Call(r.card, 0, uintptr(unsafe.Pointer(&nameLen)),
		uintptr(unsafe.Pointer(&state)), uintptr(unsafe.Pointer(&proto)),
		0, uintptr(unsafe.Pointer(&atrLen)))
	return rv == scardSuccess && state&scardStatePresent != 0
}

// ATR returns the Answer-To-Reset bytes of the inserted card.
func (r *WinPCSCReader) ATR() []byte { return r.atr }

// Name returns the connected reader's name.
func (r *WinPCSCReader) Name() string { return r.name }

// Close disconnects the card and releases the PC/SC context.
func (r *WinPCSCReader) Close() error {
	if r.card != 0 {
		procDisconnect.Call(r.card, uintptr(scardLeaveCard))
		r.card = 0
	}
	if r.ctx != 0 {
		procReleaseContext.Call(r.ctx)
		r.ctx = 0
	}
	return nil
}

// firstMultiString returns the first NUL-terminated string from a PC/SC
// multi-string (UTF-16 strings back to back, terminated by an empty one).
func firstMultiString(buf []uint16) string {
	end := 0
	for end < len(buf) && buf[end] != 0 {
		end++
	}
	if end == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:end])
}

// Status probes the local PC/SC subsystem for a reader and card.
func Status() ReaderStatus {
	r, err := NewWinPCSCReader()
	if err != nil {
		return ReaderStatus{Reason: err.Error()}
	}
	defer r.Close()
	st := ReaderStatus{Available: true, Reader: r.Name(), CardPresent: r.Present()}
	if st.CardPresent {
		if atr := r.ATR(); len(atr) > 0 {
			st.ATR = fmt.Sprintf("% x", atr)
		}
	}
	return st
}

// OpenReader opens a PC/SC reader for the lifetime of a redirection session.
func OpenReader() (Reader, error) {
	return NewWinPCSCReader()
}
