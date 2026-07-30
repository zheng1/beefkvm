// Package vcard implements virtual smart-card reader passthrough for
// the Avocent APCP protocol. This allows PIV/CAC authentication from
// the target system using a local smart card reader.
//
// Protocol: BMC sends VCARD_XFER_BLOCK (0x8301) with APDU commands,
// client responds with VCARD_DATA_BLOCK (0x0301) containing card responses.
package vcard

import (
	"encoding/binary"
	"fmt"
	"log"

	"github.com/zheng1/beefkvm/internal/apcp"
)

// ReaderStatus reports the local smart-card reader state for the UI.
// Available is true when a PC/SC reader is present; CardPresent adds whether a
// card is inserted. Reason explains why Available is false (no reader, no PC/SC
// on this platform, etc.).
type ReaderStatus struct {
	Available   bool   `json:"available"`
	Reader      string `json:"reader,omitempty"`
	CardPresent bool   `json:"card_present"`
	ATR         string `json:"atr,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// Reader abstracts a physical smart card reader. Implementers should
// wrap platform-specific PC/SC or CCID APIs.
type Reader interface {
	// Transmit sends an APDU to the card and returns the response.
	Transmit(apdu []byte) ([]byte, error)
	// Present returns true if a card is currently inserted.
	Present() bool
	// ATR returns the Answer-To-Reset bytes of the inserted card.
	ATR() []byte
	// Name returns the reader's human-readable name.
	Name() string
	// Close releases the reader.
	Close() error
}

// Session manages VCARD protocol over an APCP VM session.
type Session struct {
	reader Reader
	log    *log.Logger
}

// NewSession creates a VCARD session with the given reader.
func NewSession(r Reader) *Session {
	return &Session{reader: r}
}

// SetLogger sets the logger for debug output.
func (s *Session) SetLogger(l *log.Logger) { s.log = l }

// HandleVcardXferBlock processes a BMC VCARD_XFER_BLOCK (0x8301) request
// and returns a VCARD_DATA_BLOCK (0x0301) response frame.
//
// Payload layout (observed from Java client):
//
//	[0..1] : sequence number (short BE)
//	[2..5] : APDU length (int BE)
//	[6..]  : APDU bytes
func (s *Session) HandleVcardXferBlock(payload []byte) (apcp.Frame, error) {
	if len(payload) < 6 {
		return apcp.Frame{}, fmt.Errorf("vcard: XFER_BLOCK too short: %d", len(payload))
	}
	seq := binary.BigEndian.Uint16(payload[0:2])
	apduLen := int(binary.BigEndian.Uint32(payload[2:6]))
	if len(payload) < 6+apduLen {
		return apcp.Frame{}, fmt.Errorf("vcard: APDU truncated: got %d, need %d",
			len(payload)-6, apduLen)
	}
	apdu := payload[6 : 6+apduLen]

	s.debug("← VCARD_XFER_BLOCK seq=%d apdu=%x", seq, apdu)

	// Check card presence
	if !s.reader.Present() {
		// No card: return error response SW=0x6400 (execution error)
		resp := []byte{0x64, 0x00}
		return s.buildDataBlock(seq, resp), nil
	}

	// Transmit APDU to card
	resp, err := s.reader.Transmit(apdu)
	if err != nil {
		s.debug("transmit err: %v", err)
		// Return generic error SW
		resp = []byte{0x6F, 0x00}
	}

	s.debug("→ VCARD_DATA_BLOCK seq=%d resp=%x", seq, resp)
	return s.buildDataBlock(seq, resp), nil
}

// buildDataBlock constructs a VCARD_DATA_BLOCK (0x0301) frame.
// Payload layout:
//
//	[0..1] : sequence number (short BE)
//	[2..5] : response length (int BE)
//	[6..]  : response bytes (APDU response including SW1 SW2)
func (s *Session) buildDataBlock(seq uint16, resp []byte) apcp.Frame {
	body := make([]byte, 6+len(resp))
	binary.BigEndian.PutUint16(body[0:2], seq)
	binary.BigEndian.PutUint32(body[2:6], uint32(len(resp)))
	copy(body[6:], resp)
	return apcp.Frame{Type: apcp.MsgVcardDataBlock, Payload: body}
}

// SendVcardRequest sends VCARD_REQUEST (0x0213) to announce smart-card
// capability to the BMC. Payload layout:
//
//	[0..1] : reader id (short BE) — always 0 for single reader
//	[2]    : flags — 0x01 = reader present
//	[3..6] : ATR length (int BE)
//	[7..]  : ATR bytes
func (s *Session) SendVcardRequest(readerID uint16) apcp.Frame {
	var atr []byte
	flags := byte(0)
	if s.reader.Present() {
		flags = 0x01
		atr = s.reader.ATR()
	}

	body := make([]byte, 7+len(atr))
	binary.BigEndian.PutUint16(body[0:2], readerID)
	body[2] = flags
	binary.BigEndian.PutUint32(body[3:7], uint32(len(atr)))
	copy(body[7:], atr)

	s.debug("→ VCARD_REQUEST id=%d flags=0x%02x atr=%x", readerID, flags, atr)
	return apcp.Frame{Type: apcp.MsgVcardRequest, Payload: body}
}

func (s *Session) debug(format string, args ...interface{}) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}
