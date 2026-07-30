package apcp

import (
	"encoding/binary"
	"fmt"
	"io"
)

// KVM channel uses the AVPT ("BEEF" magic) frame envelope after TLS. It is
// unrelated to the AVMP envelope used by VM. Header layout (from
// com.avocent.kvm.b.a.b.a):
//
//   Offset 0..3: "BEEF" magic (0x42 0x45 0x45 0x46)
//   Offset 4..5: packet id (uint16 big-endian)
//   Offset 6..7: total packet length (uint16 big-endian, includes header)
//
// The KVM login packet body is a fixed 208 bytes (216 total). See
// KVMLoginRequest below.

// KVM AVPT (BEEF) magic bytes.
var MagicBEEF = [4]byte{'B', 'E', 'E', 'F'}

const (
	AVPTHeaderLen    = 8
	KVMLoginPktID    = 256 // com.avocent.kvm.b.a.cb no-arg ctor
	KVMLoginPktID2   = 258 // ctor arg true (has extra q byte)
	KVMLoginBodyLen  = 208
	KVMLoginTotalLen = KVMLoginBodyLen + AVPTHeaderLen

	// KVM heartbeat packet, id=1024 (0x0400), 8-byte zero body.
	// See com.avocent.kvm.b.a.q. Client should send every 10s.
	KVMHeartbeatID uint16 = 1024
)

// KVMHeartbeat returns the KVM channel heartbeat frame.
func KVMHeartbeat() []byte {
	return AVPTFrame{ID: KVMHeartbeatID, Payload: make([]byte, 8)}.Encode()
}

// AVPTFrame is a BEEF-envelope frame.
type AVPTFrame struct {
	ID      uint16
	Payload []byte
}

// Encode returns the full BEEF frame ready for the wire.
func (f AVPTFrame) Encode() []byte {
	total := AVPTHeaderLen + len(f.Payload)
	buf := make([]byte, total)
	copy(buf[0:4], MagicBEEF[:])
	binary.BigEndian.PutUint16(buf[4:6], f.ID)
	binary.BigEndian.PutUint16(buf[6:8], uint16(total))
	copy(buf[8:], f.Payload)
	return buf
}

// ReadAVPTFrame reads one AVPT frame from r. The header is 8 bytes; the
// first 4 are typically the ASCII "BEEF" magic when sent by the client but
// the BMC frequently omits it (sends zeros instead). The receiver only
// reads bytes 4..7 as packet id + length. We accept both.
func ReadAVPTFrame(r io.Reader) (AVPTFrame, error) {
	var hdr [AVPTHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return AVPTFrame{}, err
	}
	// bytes 0..3: BEEF or zeros — ignore
	id := binary.BigEndian.Uint16(hdr[4:6])
	total := binary.BigEndian.Uint16(hdr[6:8])
	if int(total) < AVPTHeaderLen {
		return AVPTFrame{}, fmt.Errorf("apcp: bogus AVPT length %d", total)
	}
	payload := make([]byte, int(total)-AVPTHeaderLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return AVPTFrame{}, err
	}
	return AVPTFrame{ID: id, Payload: payload}, nil
}

// KVMLoginRequest builds the payload for the KVM channel login packet
// (com.avocent.kvm.b.a.cb.getPayload). Field layout:
//
//	Offset  Bytes  Field
//	0       1      username length
//	1      96      username, UTF-8, zero-padded
//	97      1      password length
//	98     96      password, UTF-8, zero-padded
//	194     8      MAC address / auth token bytes (unused; zeros)
//	202     1      client protocol major (echo of SessionSetup's version)
//	203     1      client protocol minor
//	204     4      session id — big-endian; the Java client generates
//	                this via (int)(Math.random() * 1e7), unique per session
//	208     1      preempt / option (optional; only if Extended packet id 258)
//
// Payload length: 208 bytes (or 209 with extended).
type KVMLoginRequest struct {
	Username   string
	Password   string
	ProtoMajor byte // from SessionSetup.VersionMajor
	ProtoMinor byte // from SessionSetup.VersionMinor
	Session    uint32
	Extended   bool
	Option     byte
}

// KVMLoginResponse parses the fields of a login-status BEEF frame from the
// server (packet id 33536 basic or 33541 extended). Extended layout from
// com.avocent.kvm.b.a.yb.setData:
//
//	payload[0]    = status byte (see AVMPStatus constants below)
//	payload[1]    = i (unknown)
//	payload[2]    = j (protocol minor?)
//	payload[4..7] = server-assigned session id (int, big-endian, m)
//	payload[8..9] = flags short (l): bit0=p (companion socket needed),
//	                                 bit1=q, bit5=s, bit6=r (preempt)
//	payload[10]   = username-length
//	payload[11..] = username, up to 96 bytes
type KVMLoginResponse struct {
	Status    byte
	SessionID uint32
	Flags     uint16
	Username  string

	// Derived
	NeedsCompanion bool // p flag: video streams on a separate socket
}

// DecodeKVMLoginResponse parses a login-status AVPT frame. It tolerates
// both the extended (id 33541) and non-extended (33536) shapes.
func DecodeKVMLoginResponse(f AVPTFrame) KVMLoginResponse {
	r := KVMLoginResponse{}
	if len(f.Payload) < 10 {
		return r
	}
	extended := f.ID == 33541
	r.Status = f.Payload[0]
	if extended {
		if len(f.Payload) >= 8 {
			r.SessionID = binary.BigEndian.Uint32(f.Payload[4:8])
		}
		if len(f.Payload) >= 10 {
			r.Flags = binary.BigEndian.Uint16(f.Payload[8:10])
		}
		if len(f.Payload) >= 11 {
			nameLen := int(f.Payload[10])
			if 11+nameLen <= len(f.Payload) {
				r.Username = string(f.Payload[11 : 11+nameLen])
			}
		}
	} else {
		// non-extended: flags at [3], session id at [4..7]
		r.Flags = uint16(f.Payload[3])
		if len(f.Payload) >= 8 {
			r.SessionID = binary.BigEndian.Uint32(f.Payload[4:8])
		}
		if len(f.Payload) >= 9 {
			nameLen := int(f.Payload[8])
			if 9+nameLen <= len(f.Payload) {
				r.Username = string(f.Payload[9 : 9+nameLen])
			}
		}
	}
	// Per s.java block flow: e()=false → x() (companion socket path).
	// e() returns (l & 1) > 0 in yb.java, so:
	r.NeedsCompanion = r.Flags&1 == 0
	return r
}

// EncodeVideoSyncDC returns the "dc" packet (id=1) that the client sends
// on the companion video socket to associate it with the primary KVM
// session. Payload is client-random-session-id (int, BE) + server-assigned
// session id (int, BE). See com.avocent.kvm.b.a.dc.getPayload.
//
// NOTE: dc uses a different framing than regular AVPT packets. Its parent
// class cc.getHeader emits:
//
//	byte[4] = 0x01
//	byte[5] = packet id low byte
//	byte[6..7] = total length (big-endian short)
//	byte[0..3] = zero
//
// So the on-wire bytes are `00000000 01 01 0010 <8-byte payload>` for
// dc (id=1, length=16).
func EncodeVideoSyncDC(clientSessionID, serverSessionID uint32) []byte {
	buf := make([]byte, 16)
	// header: 4 zero bytes, then 0x01, packet-id, len (BE u16)
	buf[4] = 0x01
	buf[5] = 0x01 // dc packet id
	binary.BigEndian.PutUint16(buf[6:8], 16)
	binary.BigEndian.PutUint32(buf[8:12], clientSessionID)
	binary.BigEndian.PutUint32(buf[12:16], serverSessionID)
	return buf
}
func EncodeKVMLogin(l KVMLoginRequest) ([]byte, error) {
	if len(l.Username) > 96 {
		return nil, fmt.Errorf("apcp: username longer than 96")
	}
	if len(l.Password) > 96 {
		return nil, fmt.Errorf("apcp: password longer than 96")
	}
	body := make([]byte, KVMLoginBodyLen)
	body[0] = byte(len(l.Username))
	copy(body[1:97], l.Username)
	body[97] = byte(len(l.Password))
	copy(body[98:194], l.Password)
	// 194..201 zero
	body[202] = l.ProtoMajor
	body[203] = l.ProtoMinor
	binary.BigEndian.PutUint32(body[204:208], l.Session)
	id := uint16(KVMLoginPktID)
	if l.Extended {
		id = KVMLoginPktID2
		body = append(body, l.Option)
	}
	return AVPTFrame{ID: id, Payload: body}.Encode(), nil
}
