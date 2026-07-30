// Package ipmi implements enough of IPMI 2.0 / RMCP+ over UDP to run
// chassis commands (power on/off/reset, boot device override) against a
// Dell iDRAC6. Cipher suite 3 only: HMAC-SHA1 auth, HMAC-SHA1-96
// integrity, AES-CBC-128 encryption. No CGO.
//
// Wire layer overview (bottom to top):
//
//	RMCP header (4B)            0x06 0x00 0xff 0x07 — v1.5, seq FF, class IPMI
//	IPMI 2.0 session header     auth type, payload type, session ID/seq
//	Payload                     RMCP+ Open Session, RAKP1..4, or IPMI msg
//	Integrity trailer           pad, next-header, HMAC-SHA1-96 (12B) when auth
//
// Authenticated IPMI messages carry an IV (16B) + AES-encrypted LAN body
// with a confidentiality-trailer pad. See session.go for the RAKP dance.
package ipmi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// RMCP framing.
	rmcpVersion   = 0x06
	rmcpSeqACK    = 0xff
	rmcpClassIPMI = 0x07

	// Session auth-type nibble for IPMI 2.0/RMCP+.
	authTypeRMCPPlus = 0x06

	// Payload types.
	payloadIPMI            = 0x00
	payloadSOL             = 0x01
	payloadOpenSessionReq  = 0x10
	payloadOpenSessionResp = 0x11
	payloadRAKP1           = 0x12
	payloadRAKP2           = 0x13
	payloadRAKP3           = 0x14
	payloadRAKP4           = 0x15

	// Payload-type header byte bits.
	payloadBitAuth = 0x40
	payloadBitEnc  = 0x80

	// Cipher suite 3.
	authAlgHMACSHA1     = 0x01
	integrityAlgSHA1_96 = 0x01
	confAlgAESCBC128    = 0x01

	// Privilege levels.
	privUser          = 0x02
	privOperator      = 0x03
	privAdministrator = 0x04

	// Address constants for IPMI LAN messages.
	addrBMC      = 0x20 // BMC responder address
	addrSoftware = 0x81 // "software ID" for the remote console

	// NetFn values (already shifted-in-place below via netFn<<2).
	netFnChassisReq = 0x00
	netFnChassisRsp = 0x01
	netFnAppReq     = 0x06
	netFnAppRsp     = 0x07

	// Chassis commands.
	cmdGetChassisStatus = 0x01
	cmdChassisControl   = 0x02
	cmdSetBootOptions   = 0x08

	// UDP port for IPMI over LAN.
	IPMIPort = 623
)

// rmcpHeader is the 4-byte RMCP preamble present on every packet.
var rmcpHeader = []byte{rmcpVersion, 0x00, rmcpSeqACK, rmcpClassIPMI}

// buildRMCPPlus wraps a payload in an IPMI 2.0 session envelope.
//
// For pre-session RMCP+ messages (Open Session, RAKP1..4) sessionID and
// seq are both 0 and neither auth nor encryption is applied.
//
// For authenticated post-session traffic, callers must supply k1/k2 (both
// derived from SIK) plus a fresh IV; this function then encrypts and
// integrity-protects the payload in one pass.
func buildRMCPPlus(payloadType byte, sessionID, seq uint32,
	payload []byte, k1, k2 []byte, iv []byte) []byte {

	auth := k1 != nil
	enc := k2 != nil && iv != nil

	pt := payloadType
	if auth {
		pt |= payloadBitAuth
	}
	if enc {
		pt |= payloadBitEnc
	}

	body := payload
	if enc {
		body = aesEncryptPayload(k2[:16], iv, payload)
		body = append(append([]byte{}, iv...), body...)
	}

	// IPMI 2.0 session header: 12 bytes for standard payload types.
	//   [0]      auth type = 0x06 (RMCP+)
	//   [1]      payload type + auth/enc bits
	//   [2..5]   session ID (LE)
	//   [6..9]   session sequence (LE)
	//   [10..11] payload length (LE)
	hdr := make([]byte, 12)
	hdr[0] = authTypeRMCPPlus
	hdr[1] = pt
	binary.LittleEndian.PutUint32(hdr[2:6], sessionID)
	binary.LittleEndian.PutUint32(hdr[6:10], seq)
	binary.LittleEndian.PutUint16(hdr[10:12], uint16(len(body)))

	buf := make([]byte, 0, len(rmcpHeader)+len(hdr)+len(body)+32)
	buf = append(buf, rmcpHeader...)
	buf = append(buf, hdr...)
	buf = append(buf, body...)

	if auth {
		// IPMI 2.0 §13.6: integrity is computed from the AuthType byte
		// through the last byte of the "Next Header" trailer field. The
		// trailing padding aligns the range (starting at AuthType) to a
		// 4-byte boundary; the pad-length byte and next-header (0x07)
		// then follow before the 12-byte HMAC.
		intStart := len(rmcpHeader) // skip only the RMCP header
		intEnd := len(buf)
		// Length of region so far = (intEnd - intStart) + padLen + 2
		// must satisfy ((intEnd-intStart) + padLen + 2) % 4 == 0.
		padLen := (4 - ((intEnd-intStart)+2)%4) % 4
		for i := 0; i < padLen; i++ {
			buf = append(buf, 0xff)
		}
		buf = append(buf, byte(padLen))
		buf = append(buf, 0x07) // next header, always 0x07 per spec
		mac := hmacSHA1(k1, buf[intStart:])
		buf = append(buf, mac[:12]...) // HMAC-SHA1-96
	}

	return buf
}

// parseRMCPPlus reverses buildRMCPPlus. Returns the payload type nibble
// (auth/enc bits cleared) and the raw payload bytes; if the packet was
// encrypted, they are decrypted here. Integrity is verified when k1!=nil.
func parseRMCPPlus(pkt, k1, k2 []byte) (payloadType byte, sessionID uint32,
	seq uint32, payload []byte, err error) {

	if len(pkt) < len(rmcpHeader)+12 {
		err = errors.New("ipmi: packet shorter than RMCP+ header")
		return
	}
	if pkt[0] != rmcpVersion || pkt[3] != rmcpClassIPMI {
		err = fmt.Errorf("ipmi: not an RMCP/IPMI packet (%02x %02x)", pkt[0], pkt[3])
		return
	}
	off := len(rmcpHeader)
	if pkt[off] != authTypeRMCPPlus {
		err = fmt.Errorf("ipmi: unexpected auth type 0x%02x", pkt[off])
		return
	}
	ptRaw := pkt[off+1]
	auth := ptRaw&payloadBitAuth != 0
	enc := ptRaw&payloadBitEnc != 0
	payloadType = ptRaw &^ (payloadBitAuth | payloadBitEnc)
	sessionID = binary.LittleEndian.Uint32(pkt[off+2 : off+6])
	seq = binary.LittleEndian.Uint32(pkt[off+6 : off+10])
	payLen := int(binary.LittleEndian.Uint16(pkt[off+10 : off+12]))
	bodyStart := off + 12
	bodyEnd := bodyStart + payLen
	if bodyEnd > len(pkt) {
		err = fmt.Errorf("ipmi: payload length %d exceeds packet (%d)", payLen, len(pkt))
		return
	}

	if auth {
		if k1 == nil {
			err = errors.New("ipmi: authenticated packet but no k1")
			return
		}
		if len(pkt) < bodyEnd+2+12 {
			err = errors.New("ipmi: packet too short for integrity trailer")
			return
		}
		// The trailer begins immediately after the payload. Reading
		// backwards: last 12 bytes = HMAC, then next-header, then pad-len.
		macAt := len(pkt) - 12
		nextHdrAt := macAt - 1
		padLenAt := nextHdrAt - 1
		padLen := int(pkt[padLenAt])
		if bodyEnd+padLen+2 != macAt {
			err = fmt.Errorf("ipmi: integrity trailer size mismatch (pad=%d)", padLen)
			return
		}
		mac := hmacSHA1(k1, pkt[off:macAt])
		if !equalBytes(mac[:12], pkt[macAt:macAt+12]) {
			err = errors.New("ipmi: integrity check failed")
			return
		}
	}

	body := pkt[bodyStart:bodyEnd]
	if enc {
		if len(body) < 16 || k2 == nil {
			err = errors.New("ipmi: encrypted packet missing IV or k2")
			return
		}
		iv := body[:16]
		payload, err = aesDecryptPayload(k2[:16], iv, body[16:])
		return
	}
	payload = append([]byte{}, body...)
	return
}

// equalBytes is a constant-time byte compare (avoid importing crypto/subtle
// just for this — the value protected here is a MAC and we already trust
// the transport channel is UDP unicast).
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// buildLANMessage wraps an IPMI request in the LAN dual-checksum envelope.
// See IPMI spec §13.8. rqSeq is the 6-bit request sequence, wrapped by
// the caller; lun is normally 0 for chassis commands.
func buildLANMessage(netFn, rqSeq, cmd byte, data []byte) []byte {
	buf := make([]byte, 0, 7+len(data))
	buf = append(buf, addrBMC)
	buf = append(buf, netFn<<2) // lun=0
	buf = append(buf, ipmiChecksum(buf))
	buf = append(buf, addrSoftware)
	buf = append(buf, rqSeq<<2) // lun=0
	buf = append(buf, cmd)
	buf = append(buf, data...)
	buf = append(buf, ipmiChecksum(buf[3:]))
	return buf
}

// parseLANResponse strips the LAN envelope, verifies both checksums, and
// returns (netFn, cmd, completion, data). completion==0 means success;
// other values map through Table 5-2 of the spec (0xC0..0xFF).
func parseLANResponse(msg []byte) (netFn, cmd, cc byte, data []byte, err error) {
	if len(msg) < 8 {
		err = fmt.Errorf("ipmi: response too short (%d bytes)", len(msg))
		return
	}
	if ipmiChecksum(msg[:2]) != msg[2] {
		err = errors.New("ipmi: bad header checksum")
		return
	}
	if ipmiChecksum(msg[3:len(msg)-1]) != msg[len(msg)-1] {
		err = errors.New("ipmi: bad body checksum")
		return
	}
	netFn = msg[1] >> 2
	cmd = msg[5]
	cc = msg[6]
	data = msg[7 : len(msg)-1]
	return
}

// ipmiChecksum is the IPMI two's-complement running sum used in every LAN
// message header/body.
func ipmiChecksum(b []byte) byte {
	var s byte
	for _, x := range b {
		s += x
	}
	return -s
}
