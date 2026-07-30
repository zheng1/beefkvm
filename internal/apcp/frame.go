// Package apcp encodes and decodes Avocent Point-to-point Communication Protocol
// frames as spoken by ASPEED BMCs shipped in 2010–2013 Gigabyte servers.
//
// The protocol has three layered magics:
//
//   - APCP: pre-TLS session handshake (SessionRequest, SessionSetup).
//   - AVMP: post-TLS authenticated stream, used by both KVM and VM channels.
//   - AVPT: KVM-specific inner message envelope carried inside AVMP payloads.
//
// This package is pure encoding + state. Network I/O and logging live in the
// callers so the same code drives probe, MITM proxy, and future frontend
// bridge equally.
package apcp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic bytes for the three framing layers.
var (
	MagicAPCP = [4]byte{'A', 'P', 'C', 'P'}
	MagicAVMP = [4]byte{'A', 'V', 'M', 'P'}
	MagicAVPT = [4]byte{'A', 'V', 'P', 'T'}
)

const (
	APCPHeaderLen = 10
	AVMPHeaderLen = 10

	SessionRequestLen = 53
	LoginLen          = 153

	// SessionSetupStatusOK is the fixed status field in a valid SessionSetup
	// response (also reused in LoginStatus). Java source encodes it as -32512
	// via readShort; the wire form is 0x8100.
	SessionSetupStatusOK uint16 = 0x8100

	// Protocol version emitted by the client for both SessionRequest and
	// Login. The two bytes represent major/minor (1.0).
	ProtoVersion uint16 = 0x0100

	// HeartbeatIntervalMs matches HeartBeat.MAX_HEARTBEAT_INTERVAL.
	HeartbeatIntervalMs = 10_000
)

// SessionRequest fields, built with EncodeSessionRequest.
type SessionRequest struct {
	SessionType byte     // Java calls it "byte a"; VM sends 2, KVM sends 2 too.
	Field2      byte     // reserved-ish flags
	Field3      byte     // reserved-ish flags
	Capability  uint32   // VM: 5. Requests the caps we can accept.
	Nonce       [32]byte // client random; up to 32 bytes are copied in.
	NonceLen    byte     // length actually populated in Nonce
}

// EncodeSessionRequest builds the 53-byte APCP SessionRequest frame.
func EncodeSessionRequest(r SessionRequest) []byte {
	buf := make([]byte, SessionRequestLen)
	copy(buf[0:4], MagicAPCP[:])
	binary.BigEndian.PutUint32(buf[4:8], SessionRequestLen)
	binary.BigEndian.PutUint16(buf[8:10], ProtoVersion)
	// bytes 10..11 zero
	buf[12] = r.SessionType
	buf[13] = r.Field2
	buf[14] = r.Field3
	// buf[15] zero
	binary.BigEndian.PutUint32(buf[16:20], r.Capability)
	buf[20] = r.NonceLen
	copy(buf[21:53], r.Nonce[:])
	return buf
}

// SessionSetup fields returned by the BMC after SessionRequest.
type SessionSetup struct {
	Status       uint16
	VersionMajor byte
	VersionMinor byte
	Capabilities uint32
	TCPPort      uint16
	NonceLen     byte
	Nonce        [32]byte
}

// SupportsSSL reports whether the SSL bit is set in the returned capabilities.
// From VirtualMedia.java switch (getConnectionCapabilities()) case 4.
func (s SessionSetup) SupportsSSL() bool { return s.Capabilities&0x04 != 0 }

// DecodeSessionSetup parses the APCP SessionSetup frame. It reads exactly
// SessionRequestLen bytes (the response is the same 53-byte layout as the
// request, just with different semantics for the trailing fields).
func DecodeSessionSetup(buf []byte) (SessionSetup, error) {
	if len(buf) < SessionRequestLen {
		return SessionSetup{}, fmt.Errorf("apcp: session setup too short: %d", len(buf))
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != MagicAPCP {
		return SessionSetup{}, fmt.Errorf("apcp: bad magic %x", buf[0:4])
	}
	status := binary.BigEndian.Uint16(buf[8:10])
	if status != SessionSetupStatusOK {
		return SessionSetup{}, fmt.Errorf("apcp: session setup status 0x%04x, want 0x%04x", status, SessionSetupStatusOK)
	}
	s := SessionSetup{
		Status:       status,
		VersionMajor: buf[12],
		VersionMinor: buf[13],
		Capabilities: binary.BigEndian.Uint32(buf[14:18]),
		TCPPort:      binary.BigEndian.Uint16(buf[18:20]),
		NonceLen:     buf[20],
	}
	copy(s.Nonce[:], buf[21:53])
	return s, nil
}

// LoginRequest fields.
type LoginRequest struct {
	Username string
	Password string
	Preempt  bool
}

// EncodeLogin builds the 153-byte AVMP Login frame. Password is plaintext;
// TLS is required.
func EncodeLogin(l LoginRequest) ([]byte, error) {
	if len(l.Username) > 96 {
		return nil, errors.New("apcp: username longer than 96 bytes")
	}
	if len(l.Password) > 32 {
		return nil, errors.New("apcp: password longer than 32 bytes")
	}
	buf := make([]byte, LoginLen)
	copy(buf[0:4], MagicAVMP[:])
	binary.BigEndian.PutUint32(buf[4:8], LoginLen)
	binary.BigEndian.PutUint16(buf[8:10], ProtoVersion)
	// buf[10..11] zero
	buf[12] = byte(len(l.Username))
	copy(buf[13:109], l.Username)
	copy(buf[109:141], l.Password)
	// buf[141..148] zero (8 bytes)
	// buf[149..151] zero (flags 1..3)
	if l.Preempt {
		buf[152] = 1
	}
	return buf, nil
}

// LoginStatus fields returned by the BMC.
type LoginStatus struct {
	Status              byte
	CanRejectPreemption bool
	Username            string
}

// LoginStatusCode names for the byte returned in a LoginStatus. From
// com.avocent.vm.AVMPStatus.
var LoginStatusCode = map[byte]string{
	0:   "SUCCEEDED",
	1:   "INVALID_USER_NAME",
	2:   "INVALID_PASSWORD",
	3:   "CHANNEL_ACCESS_DENIED",
	4:   "CHANNEL_IN_USE",
	5:   "CHANNEL_NOT_FOUND",
	6:   "SERVER_NOT_AVAILABLE_CAN_PREEMPT",
	8:   "ALL_CHANNELS_IN_USE",
	11:  "CHANNEL_IN_USE_BY_LOCAL_USER_CAN_PREEMPT",
	22:  "NETWORK_AUTH_SERVER_ERROR",
	23:  "INVALID_EXPIRED_CERT_ERROR",
	52:  "PREEMPT_REJECTED",
	255: "GENERIC_FAILED", // Java uses -1
}

// LoginStatusName returns a human-readable string for the login status byte.
func LoginStatusName(b byte) string {
	if name, ok := LoginStatusCode[b]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", b)
}

// DecodeLoginStatus parses the AVMP LoginStatus response. Format: 4-byte
// magic, 4-byte length (uint32), 2-byte status field (must equal
// SessionSetupStatusOK), 2 reserved, 1 status byte, 1 flags, 1 name-length,
// 96 bytes username padded.
func DecodeLoginStatus(buf []byte) (LoginStatus, error) {
	if len(buf) < 111 {
		return LoginStatus{}, fmt.Errorf("apcp: login status too short: %d", len(buf))
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != MagicAVMP {
		return LoginStatus{}, fmt.Errorf("apcp: bad AVMP magic in LoginStatus: %x", buf[0:4])
	}
	status := binary.BigEndian.Uint16(buf[8:10])
	if status != SessionSetupStatusOK {
		return LoginStatus{}, fmt.Errorf("apcp: login status status 0x%04x, want 0x%04x", status, SessionSetupStatusOK)
	}
	ls := LoginStatus{
		Status:              buf[12],
		CanRejectPreemption: buf[13]&1 != 0,
	}
	nameLen := int(buf[14])
	if nameLen > 96 {
		nameLen = 96
	}
	if 15+nameLen <= len(buf) {
		ls.Username = string(buf[15 : 15+nameLen])
	}
	return ls, nil
}

// Frame represents one AVMP-layer message on the post-login stream. This is
// how heartbeats and all subsequent traffic are encoded.
type Frame struct {
	Type    uint16
	Payload []byte
}

// Encode returns the full AVMP frame (header + payload) ready for the wire.
func (f Frame) Encode() []byte {
	total := AVMPHeaderLen + len(f.Payload)
	buf := make([]byte, total)
	copy(buf[0:4], MagicAVMP[:])
	binary.BigEndian.PutUint32(buf[4:8], uint32(total))
	binary.BigEndian.PutUint16(buf[8:10], f.Type)
	copy(buf[10:], f.Payload)
	return buf
}

// ReadFrame reads one AVMP frame from r. It returns io.EOF cleanly when the
// stream ends at a frame boundary.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [AVMPHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	if [4]byte{hdr[0], hdr[1], hdr[2], hdr[3]} != MagicAVMP {
		return Frame{}, fmt.Errorf("apcp: bad AVMP magic: %x", hdr[0:4])
	}
	total := binary.BigEndian.Uint32(hdr[4:8])
	if total < AVMPHeaderLen {
		return Frame{}, fmt.Errorf("apcp: bogus frame length %d", total)
	}
	msgType := binary.BigEndian.Uint16(hdr[8:10])
	payload := make([]byte, int(total)-AVMPHeaderLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Type: msgType, Payload: payload}, nil
}

// Heartbeat produces the 10-byte AVMP heartbeat frame.
func Heartbeat() Frame { return Frame{Type: MsgHeartbeat} }
