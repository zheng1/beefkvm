//go:build darwin && cgo
// +build darwin,cgo

package vcard

// #cgo LDFLAGS: -framework PCSC
// #include <PCSC/winscard.h>
// #include <PCSC/wintypes.h>
// #include <stdlib.h>
import "C"
import (
	"fmt"
	"unsafe"
)

// PCSCReader wraps macOS PC/SC framework for smart card access.
type PCSCReader struct {
	ctx     C.SCARDCONTEXT
	card    C.SCARDHANDLE
	proto   C.DWORD
	readers []byte
	atr     []byte
}

// NewPCSCReader initializes a PC/SC reader context and connects to the
// first available reader with a card present.
func NewPCSCReader() (*PCSCReader, error) {
	r := &PCSCReader{}

	// Establish context
	rv := C.SCardEstablishContext(C.SCARD_SCOPE_SYSTEM, nil, nil, &r.ctx)
	if rv != C.SCARD_S_SUCCESS {
		return nil, fmt.Errorf("SCardEstablishContext: 0x%08x", uint32(rv))
	}

	// List readers
	var readersLen C.DWORD
	rv = C.SCardListReaders(r.ctx, nil, nil, &readersLen)
	if rv != C.SCARD_S_SUCCESS {
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("SCardListReaders: 0x%08x", uint32(rv))
	}

	r.readers = make([]byte, readersLen)
	rv = C.SCardListReaders(r.ctx, nil, (*C.char)(unsafe.Pointer(&r.readers[0])), &readersLen)
	if rv != C.SCARD_S_SUCCESS {
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("SCardListReaders: 0x%08x", uint32(rv))
	}

	// Connect to first reader (multi-reader support would iterate here)
	readerName := C.CString(string(r.readers[:cStringLen(r.readers)]))
	defer C.free(unsafe.Pointer(readerName))

	var activeProto C.DWORD
	rv = C.SCardConnect(r.ctx, readerName, C.SCARD_SHARE_SHARED,
		C.SCARD_PROTOCOL_T0|C.SCARD_PROTOCOL_T1, &r.card, &activeProto)
	if rv != C.SCARD_S_SUCCESS {
		// No card present is OK — we'll check later
		// SCARD_E_NO_SMARTCARD = 0x8010000C, SCARD_W_REMOVED_CARD = 0x80100069
		if uint32(rv) == 0x8010000C || uint32(rv) == 0x80100069 {
			return r, nil
		}
		C.SCardReleaseContext(r.ctx)
		return nil, fmt.Errorf("SCardConnect: 0x%08x", uint32(rv))
	}
	r.proto = activeProto

	// Read ATR
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
func (r *PCSCReader) Transmit(apdu []byte) ([]byte, error) {
	if r.card == 0 {
		return nil, fmt.Errorf("no card connected")
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

// Present returns true if a card is currently inserted.
func (r *PCSCReader) Present() bool {
	if r.card == 0 {
		return false
	}
	var state C.DWORD
	rv := C.SCardStatus(r.card, nil, nil, &state, nil, nil, nil)
	return rv == C.SCARD_S_SUCCESS && (state&C.SCARD_PRESENT != 0)
}

// ATR returns the Answer-To-Reset bytes.
func (r *PCSCReader) ATR() []byte {
	return r.atr
}

// Close releases the PC/SC context.
func (r *PCSCReader) Close() error {
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

// Name returns the connected reader's name.
func (r *PCSCReader) Name() string {
	if len(r.readers) == 0 {
		return ""
	}
	return string(r.readers[:cStringLen(r.readers)])
}

// Status probes the local PC/SC subsystem for a reader + card, opening and
// immediately closing a transient context. Used by the UI status endpoint.
func Status() ReaderStatus {
	r, err := NewPCSCReader()
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
	return NewPCSCReader()
}

// cStringLen returns the length of a null-terminated C string.
func cStringLen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
