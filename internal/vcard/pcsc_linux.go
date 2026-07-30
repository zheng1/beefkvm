//go:build linux && cgo

package vcard

// Linux PC/SC backend, built on pcsclite. The API is the PC/SC Workgroup one,
// so this mirrors the macOS backend almost line for line; the differences are
// the header location (<PCSC/winscard.h> from pcsclite rather than the macOS
// framework) and linking against libpcsclite instead of -framework PCSC.
//
// Requires the pcsclite development headers at build time:
//
//	Debian/Ubuntu:  apt install libpcsclite-dev
//	Fedora/RHEL:    dnf install pcsc-lite-devel
//	Alpine:         apk add pcsc-lite-dev
//
// and the pcscd daemon running at runtime. Builds with CGO_ENABLED=0 fall back
// to the stub, so a static binary simply reports smart-card support as
// unavailable rather than failing to build.

// #cgo pkg-config: libpcsclite
// #include <PCSC/winscard.h>
// #include <PCSC/wintypes.h>
// #include <stdlib.h>
import "C"
import (
	"fmt"
	"unsafe"
)

// LinuxPCSCReader wraps pcsclite for smart card access.
type LinuxPCSCReader struct {
	ctx     C.SCARDCONTEXT
	card    C.SCARDHANDLE
	proto   C.DWORD
	readers []byte
	atr     []byte
}

// NewLinuxPCSCReader establishes a pcsclite context and connects to the first
// available reader. A reader with no card is not an error — Present() reports
// that separately, matching the other backends.
func NewLinuxPCSCReader() (*LinuxPCSCReader, error) {
	r := &LinuxPCSCReader{}

	rv := C.SCardEstablishContext(C.SCARD_SCOPE_SYSTEM, nil, nil, &r.ctx)
	if rv != C.SCARD_S_SUCCESS {
		return nil, fmt.Errorf("SCardEstablishContext: 0x%08x", uint32(rv))
	}

	var readersLen C.DWORD
	rv = C.SCardListReaders(r.ctx, nil, nil, &readersLen)
	if rv != C.SCARD_S_SUCCESS {
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("SCardListReaders: 0x%08x", uint32(rv))
	}
	if readersLen == 0 {
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("no smart-card readers attached")
	}

	r.readers = make([]byte, readersLen)
	rv = C.SCardListReaders(r.ctx, nil, (*C.char)(unsafe.Pointer(&r.readers[0])), &readersLen)
	if rv != C.SCARD_S_SUCCESS {
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("SCardListReaders: 0x%08x", uint32(rv))
	}

	readerName := C.CString(string(r.readers[:cStringLen(r.readers)]))
	defer C.free(unsafe.Pointer(readerName))

	var activeProto C.DWORD
	rv = C.SCardConnect(r.ctx, readerName, C.SCARD_SHARE_SHARED,
		C.SCARD_PROTOCOL_T0|C.SCARD_PROTOCOL_T1, &r.card, &activeProto)
	if rv != C.SCARD_S_SUCCESS {
		// SCARD_E_NO_SMARTCARD / SCARD_W_REMOVED_CARD: reader but no card.
		if uint32(rv) == 0x8010000C || uint32(rv) == 0x80100069 {
			return r, nil
		}
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("SCardConnect: 0x%08x", uint32(rv))
	}
	r.proto = activeProto

	var atrLen C.DWORD = C.MAX_ATR_SIZE
	atrBuf := make([]byte, C.MAX_ATR_SIZE)
	var state, protocol C.DWORD
	rv = C.SCardStatus(r.card, nil, nil, &state, &protocol,
		(*C.BYTE)(unsafe.Pointer(&atrBuf[0])), &atrLen)
	if rv == C.SCARD_S_SUCCESS {
		r.atr = atrBuf[:atrLen]
	}
	return r, nil
}

// Transmit sends an APDU to the card and returns the response.
func (r *LinuxPCSCReader) Transmit(apdu []byte) ([]byte, error) {
	if r.card == 0 {
		return nil, fmt.Errorf("no card connected")
	}
	if len(apdu) == 0 {
		return nil, fmt.Errorf("empty APDU")
	}

	var sendPci C.SCARD_IO_REQUEST
	if r.proto == C.SCARD_PROTOCOL_T0 {
		sendPci = C.SCARD_IO_REQUEST{C.SCARD_PROTOCOL_T0, C.sizeof_SCARD_IO_REQUEST}
	} else {
		sendPci = C.SCARD_IO_REQUEST{C.SCARD_PROTOCOL_T1, C.sizeof_SCARD_IO_REQUEST}
	}

	respBuf := make([]byte, 258) // max short APDU response
	respLen := C.DWORD(len(respBuf))

	rv := C.SCardTransmit(r.card, &sendPci,
		(*C.BYTE)(unsafe.Pointer(&apdu[0])), C.DWORD(len(apdu)),
		nil, (*C.BYTE)(unsafe.Pointer(&respBuf[0])), &respLen)
	if rv != C.SCARD_S_SUCCESS {
		return nil, fmt.Errorf("SCardTransmit: 0x%08x", uint32(rv))
	}
	return respBuf[:respLen], nil
}

// Present reports whether a card is currently inserted.
func (r *LinuxPCSCReader) Present() bool {
	if r.card == 0 {
		return false
	}
	var state C.DWORD
	rv := C.SCardStatus(r.card, nil, nil, &state, nil, nil, nil)
	return rv == C.SCARD_S_SUCCESS && (state&C.SCARD_PRESENT != 0)
}

// ATR returns the Answer-To-Reset bytes.
func (r *LinuxPCSCReader) ATR() []byte { return r.atr }

// Name returns the connected reader's name.
func (r *LinuxPCSCReader) Name() string {
	if len(r.readers) == 0 {
		return ""
	}
	return string(r.readers[:cStringLen(r.readers)])
}

// Close disconnects the card and releases the pcsclite context.
func (r *LinuxPCSCReader) Close() error {
	if r.card != 0 {
		C.SCardDisconnect(r.card, C.SCARD_LEAVE_CARD)
		r.card = 0
	}
	if r.ctx != 0 {
		C.SCardReleaseContext(r.ctx)
		r.ctx = 0
	}
	return nil
}

// Status probes the local PC/SC subsystem for a reader and card.
func Status() ReaderStatus {
	r, err := NewLinuxPCSCReader()
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
	return NewLinuxPCSCReader()
}
