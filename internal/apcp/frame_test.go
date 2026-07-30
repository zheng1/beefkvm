package apcp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"testing"
)

// TestEncodeSessionRequest exercises the 53-byte SessionRequest against the
// exact byte layout produced by the Java client
// (com.avocent.vm.VirtualMedia.sendSessionRequest).
func TestEncodeSessionRequest(t *testing.T) {
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	req := SessionRequest{
		SessionType: 2,
		Capability:  5,
		Nonce:       nonce,
		NonceLen:    32,
	}
	got := EncodeSessionRequest(req)
	if len(got) != SessionRequestLen {
		t.Fatalf("len = %d, want %d", len(got), SessionRequestLen)
	}
	if string(got[0:4]) != "APCP" {
		t.Fatalf("magic = %q", got[0:4])
	}
	if binary.BigEndian.Uint32(got[4:8]) != SessionRequestLen {
		t.Errorf("frame length field = %d", binary.BigEndian.Uint32(got[4:8]))
	}
	if binary.BigEndian.Uint16(got[8:10]) != ProtoVersion {
		t.Errorf("version = 0x%04x", binary.BigEndian.Uint16(got[8:10]))
	}
	if got[12] != 2 {
		t.Errorf("session type = %d", got[12])
	}
	if binary.BigEndian.Uint32(got[16:20]) != 5 {
		t.Errorf("capability = %d", binary.BigEndian.Uint32(got[16:20]))
	}
	if got[20] != 32 {
		t.Errorf("nonce length byte = %d", got[20])
	}
	if !bytes.Equal(got[21:53], nonce[:]) {
		t.Errorf("nonce mismatch: %s", hex.EncodeToString(got[21:53]))
	}
}

func TestDecodeSessionSetup(t *testing.T) {
	// Build a synthetic BMC response matching the Java parser
	// (VirtualMedia.receiveSessionSetup).
	buf := make([]byte, SessionRequestLen)
	copy(buf[0:4], "APCP")
	binary.BigEndian.PutUint32(buf[4:8], SessionRequestLen)
	binary.BigEndian.PutUint16(buf[8:10], SessionSetupStatusOK)
	buf[12] = 2                               // version major
	buf[13] = 3                               // version minor
	binary.BigEndian.PutUint32(buf[14:18], 4) // caps: SSL only
	binary.BigEndian.PutUint16(buf[18:20], 2068)
	buf[20] = 32
	for i := 0; i < 32; i++ {
		buf[21+i] = byte(0x80 + i)
	}
	s, err := DecodeSessionSetup(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !s.SupportsSSL() {
		t.Errorf("SupportsSSL = false, want true")
	}
	if s.VersionMajor != 2 || s.VersionMinor != 3 {
		t.Errorf("version = %d.%d", s.VersionMajor, s.VersionMinor)
	}
	if s.TCPPort != 2068 {
		t.Errorf("tcp port = %d", s.TCPPort)
	}
	if s.NonceLen != 32 {
		t.Errorf("nonce len = %d", s.NonceLen)
	}
	if s.Nonce[0] != 0x80 || s.Nonce[31] != 0x9f {
		t.Errorf("nonce bounds: first=%x last=%x", s.Nonce[0], s.Nonce[31])
	}
}

func TestEncodeLogin(t *testing.T) {
	buf, err := EncodeLogin(LoginRequest{
		Username: "admin",
		Password: "password",
		Preempt:  true,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(buf) != LoginLen {
		t.Fatalf("len = %d, want %d", len(buf), LoginLen)
	}
	if string(buf[0:4]) != "AVMP" {
		t.Errorf("magic = %q", buf[0:4])
	}
	if binary.BigEndian.Uint32(buf[4:8]) != LoginLen {
		t.Errorf("length field = %d", binary.BigEndian.Uint32(buf[4:8]))
	}
	if binary.BigEndian.Uint16(buf[8:10]) != ProtoVersion {
		t.Errorf("version = 0x%04x", binary.BigEndian.Uint16(buf[8:10]))
	}
	if buf[12] != byte(len("admin")) {
		t.Errorf("username length = %d", buf[12])
	}
	if string(buf[13:18]) != "admin" {
		t.Errorf("username = %q", buf[13:18])
	}
	if string(buf[109:117]) != "password" {
		t.Errorf("password field = %q", buf[109:117])
	}
	if buf[152] != 1 {
		t.Errorf("preempt flag = %d", buf[152])
	}
}

func TestDecodeLoginStatus(t *testing.T) {
	// 111-byte response: 10 header + 1 status + 1 flag + 1 name-len + 96 padded name.
	buf := make([]byte, 111)
	copy(buf[0:4], "AVMP")
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(buf)))
	binary.BigEndian.PutUint16(buf[8:10], SessionSetupStatusOK)
	buf[12] = 3 // status = CHANNEL_ACCESS_DENIED
	buf[13] = 1
	buf[14] = 5 // name len
	copy(buf[15:20], "admin")
	ls, err := DecodeLoginStatus(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ls.Status != 3 || LoginStatusName(ls.Status) != "CHANNEL_ACCESS_DENIED" {
		t.Errorf("status = %d (%s)", ls.Status, LoginStatusName(ls.Status))
	}
	if !ls.CanRejectPreemption {
		t.Errorf("preempt reject flag lost")
	}
	if ls.Username != "admin" {
		t.Errorf("username = %q", ls.Username)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	f := Frame{Type: MsgHeartbeat, Payload: nil}
	enc := f.Encode()
	if len(enc) != AVMPHeaderLen {
		t.Errorf("heartbeat len = %d, want 10", len(enc))
	}
	got, err := ReadFrame(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Type != MsgHeartbeat {
		t.Errorf("type = 0x%04x", got.Type)
	}
	if len(got.Payload) != 0 {
		t.Errorf("payload len = %d", len(got.Payload))
	}
}

func TestFrameEOF(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(nil))
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}
