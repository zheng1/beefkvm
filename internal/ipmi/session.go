package ipmi

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// Session holds the RMCP+ state for one authenticated IPMI conversation.
// Not safe for concurrent use — callers hold a mutex around Send.
type Session struct {
	conn        *net.UDPConn
	addr        *net.UDPAddr
	sessionID   uint32 // managed-system session ID (assigned by BMC)
	consoleID   uint32 // remote-console session ID (we pick)
	sik, k1, k2 []byte // derived keys
	outSeq      uint32 // next outbound session sequence number
	rqSeq       byte   // 6-bit LAN request sequence, wraps
	timeout     time.Duration
	privWarn    string // non-empty if raising to Administrator returned a cc
}

// Dial establishes an authenticated RMCP+ session with cipher suite 3
// (HMAC-SHA1 / HMAC-SHA1-96 / AES-CBC-128) and Administrator privilege.
// On failure it returns the RAKP status code inside the error string.
func Dial(host, user, pass string) (*Session, error) {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprint(IPMIPort)))
	if err != nil {
		return nil, fmt.Errorf("ipmi: resolve: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("ipmi: dial: %w", err)
	}
	s := &Session{
		conn:    conn,
		addr:    addr,
		timeout: 2 * time.Second,
	}
	if err := s.rakp(user, pass); err != nil {
		conn.Close()
		return nil, err
	}
	// Raise the operating privilege to Administrator. After RAKP the session
	// often sits at a lower effective level, and privileged payload/config
	// commands (SOL Activate Payload, Set System Boot Options) then reject with
	// completion code 0xD4. Best-effort: sensor/SEL reads work at lower levels,
	// so a failure here shouldn't kill the session — the command surfaces its
	// own cc if escalation truly didn't take.
	if cc, _, err := s.rawCall(netFnAppReq, 0x3B, []byte{privAdministrator}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ipmi: set session privilege: %w", err)
	} else if cc != 0 {
		// Non-fatal, but note it.
		s.privWarn = fmt.Sprintf("set-session-privilege cc=0x%02x", cc)
	}
	return s, nil
}

// Close tears down the session and closes the UDP socket.
func (s *Session) Close() error {
	if s.sessionID != 0 {
		// Best-effort session close via App / Close Session (NetFn=0x06 0x3C).
		body := make([]byte, 4)
		binary.LittleEndian.PutUint32(body, s.sessionID)
		_, _, _ = s.rawCall(netFnAppReq, 0x3C, body)
	}
	return s.conn.Close()
}

// rakp runs the four-message RAKP handshake plus the preceding
// Open-Session exchange. On success, populates sessionID/sik/k1/k2.
func (s *Session) rakp(user, pass string) error {
	// --- Open Session Request (payload 0x10) ---
	var cid [4]byte
	if _, err := rand.Read(cid[:]); err != nil {
		return fmt.Errorf("ipmi: rand: %w", err)
	}
	s.consoleID = binary.LittleEndian.Uint32(cid[:])

	openReq := make([]byte, 0, 32)
	openReq = append(openReq, 0x00, privAdministrator, 0x00, 0x00) // tag=0, priv=admin, reserved
	openReq = append(openReq, cid[:]...)
	// Auth alg payload: type=0, reserved, length=8, alg=HMAC-SHA1
	openReq = append(openReq, 0x00, 0x00, 0x00, 0x08, authAlgHMACSHA1, 0x00, 0x00, 0x00)
	// Integrity alg payload: type=1, reserved, length=8, alg=SHA1-96
	openReq = append(openReq, 0x01, 0x00, 0x00, 0x08, integrityAlgSHA1_96, 0x00, 0x00, 0x00)
	// Confidentiality alg payload: type=2, reserved, length=8, alg=AES-CBC-128
	openReq = append(openReq, 0x02, 0x00, 0x00, 0x08, confAlgAESCBC128, 0x00, 0x00, 0x00)

	openResp, err := s.roundtrip(payloadOpenSessionReq, 0, 0, openReq, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("ipmi: open session: %w", err)
	}
	if len(openResp) < 36 {
		return fmt.Errorf("ipmi: open-session response too short (%d)", len(openResp))
	}
	if st := openResp[1]; st != 0 {
		return fmt.Errorf("ipmi: open-session status 0x%02x (%s)", st, rakpStatusName(st))
	}
	s.sessionID = binary.LittleEndian.Uint32(openResp[8:12])
	if s.sessionID == 0 {
		return fmt.Errorf("ipmi: open-session returned zero session ID")
	}

	// --- RAKP Message 1 (payload 0x12) ---
	var consoleRand [16]byte
	if _, err := rand.Read(consoleRand[:]); err != nil {
		return err
	}
	uname := []byte(user)
	rakp1 := make([]byte, 0, 28+len(uname))
	rakp1 = append(rakp1, 0x00, 0x00, 0x00, 0x00) // tag + reserved
	rakp1 = append(rakp1, byte(s.sessionID), byte(s.sessionID>>8),
		byte(s.sessionID>>16), byte(s.sessionID>>24))
	rakp1 = append(rakp1, consoleRand[:]...)
	rakp1 = append(rakp1, privAdministrator|0x10) // 0x10 = "name-only lookup"
	rakp1 = append(rakp1, 0x00, 0x00)
	rakp1 = append(rakp1, byte(len(uname)))
	rakp1 = append(rakp1, uname...)

	rakp2, err := s.roundtrip(payloadRAKP1, 0, 0, rakp1, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("ipmi: RAKP1: %w", err)
	}
	// Short response usually means the BMC rejected the session and only
	// returned a header + status code — surface that status.
	if len(rakp2) >= 2 {
		if st := rakp2[1]; st != 0 {
			return fmt.Errorf("ipmi: RAKP2 status 0x%02x (%s), body=%d bytes", st, rakpStatusName(st), len(rakp2))
		}
	}
	if len(rakp2) < 40 {
		return fmt.Errorf("ipmi: RAKP2 response too short (%d)", len(rakp2))
	}
	var bmcRand [16]byte
	copy(bmcRand[:], rakp2[8:24])
	var bmcGUID [16]byte
	copy(bmcGUID[:], rakp2[24:40])

	// Verify BMC's RAKP2 auth code, when present.
	//   HMAC-SHA1(K_UID, ConsoleSID_LE || BMCSID_LE || RC || RS || GUID || Priv || Ulen || Uname)
	kuid := padKey(pass)
	rakp2Data := make([]byte, 0, 48+len(uname))
	rakp2Data = binary.LittleEndian.AppendUint32(rakp2Data, s.consoleID)
	rakp2Data = binary.LittleEndian.AppendUint32(rakp2Data, s.sessionID)
	rakp2Data = append(rakp2Data, consoleRand[:]...)
	rakp2Data = append(rakp2Data, bmcRand[:]...)
	rakp2Data = append(rakp2Data, bmcGUID[:]...)
	rakp2Data = append(rakp2Data, privAdministrator|0x10)
	rakp2Data = append(rakp2Data, byte(len(uname)))
	rakp2Data = append(rakp2Data, uname...)
	expected := hmacSHA1(kuid, rakp2Data)
	if len(rakp2) >= 40+20 {
		if !equalBytes(expected, rakp2[40:60]) {
			return fmt.Errorf("ipmi: RAKP2 auth code mismatch (bad password?)")
		}
	}

	// --- RAKP Message 3 (payload 0x14) ---
	//   HMAC-SHA1(K_UID, RS || ConsoleSID_LE || Priv || Ulen || Uname)
	rakp3Auth := make([]byte, 0, 22+len(uname))
	rakp3Auth = append(rakp3Auth, bmcRand[:]...)
	rakp3Auth = binary.LittleEndian.AppendUint32(rakp3Auth, s.consoleID)
	rakp3Auth = append(rakp3Auth, privAdministrator|0x10)
	rakp3Auth = append(rakp3Auth, byte(len(uname)))
	rakp3Auth = append(rakp3Auth, uname...)
	rakp3AuthCode := hmacSHA1(kuid, rakp3Auth)

	rakp3 := make([]byte, 0, 8+len(rakp3AuthCode))
	rakp3 = append(rakp3, 0x00, 0x00, 0x00, 0x00) // tag + reserved
	rakp3 = binary.LittleEndian.AppendUint32(rakp3, s.sessionID)
	rakp3 = append(rakp3, rakp3AuthCode...)

	rakp4, err := s.roundtrip(payloadRAKP3, 0, 0, rakp3, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("ipmi: RAKP3: %w", err)
	}
	if len(rakp4) < 8 {
		return fmt.Errorf("ipmi: RAKP4 too short (%d)", len(rakp4))
	}
	if st := rakp4[1]; st != 0 {
		return fmt.Errorf("ipmi: RAKP4 status 0x%02x (%s)", st, rakpStatusName(st))
	}

	// Derive SIK. Kg == null → use K_UID.
	sikInput := make([]byte, 0, 34+len(uname))
	sikInput = append(sikInput, consoleRand[:]...)
	sikInput = append(sikInput, bmcRand[:]...)
	sikInput = append(sikInput, privAdministrator|0x10)
	sikInput = append(sikInput, byte(len(uname)))
	sikInput = append(sikInput, uname...)
	s.sik = hmacSHA1(kuid, sikInput)

	// Verify RAKP4 ICV: HMAC-SHA1(SIK, RC || BMCSID_LE || GUID)[:12]
	icvInput := make([]byte, 0, 36)
	icvInput = append(icvInput, consoleRand[:]...)
	icvInput = binary.LittleEndian.AppendUint32(icvInput, s.sessionID)
	icvInput = append(icvInput, bmcGUID[:]...)
	icvExpected := hmacSHA1(s.sik, icvInput)
	if len(rakp4) >= 8+12 {
		if !equalBytes(icvExpected[:12], rakp4[8:20]) {
			return fmt.Errorf("ipmi: RAKP4 ICV mismatch")
		}
	}

	// K1 and K2 for post-session traffic.
	const20 := func(v byte) []byte {
		b := make([]byte, 20)
		for i := range b {
			b[i] = v
		}
		return b
	}
	s.k1 = hmacSHA1(s.sik, const20(0x01))
	s.k2 = hmacSHA1(s.sik, const20(0x02))
	s.outSeq = 1
	return nil
}

// padKey right-pads pass to 20 bytes (SHA-1 block-aligned K_UID form).
func padKey(pass string) []byte {
	k := make([]byte, 20)
	copy(k, pass)
	return k
}

// roundtrip sends payload as a single RMCP+ packet and reads one reply
// with retries + backoff. authKeys are nil for pre-session RAKP traffic.
func (s *Session) roundtrip(payloadType byte, sessionID, seq uint32,
	payload, k1, k2, iv []byte) ([]byte, error) {

	pkt := buildRMCPPlus(payloadType, sessionID, seq, payload, k1, k2, iv)

	backoff := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	buf := make([]byte, 2048)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := s.conn.Write(pkt); err != nil {
			lastErr = err
			time.Sleep(backoff[attempt])
			continue
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(s.timeout))
		n, err := s.conn.Read(buf)
		if err != nil {
			lastErr = err
			time.Sleep(backoff[attempt])
			continue
		}
		_, _, _, payloadOut, perr := parseRMCPPlus(buf[:n], k1, k2)
		if perr != nil {
			lastErr = perr
			time.Sleep(backoff[attempt])
			continue
		}
		return payloadOut, nil
	}
	return nil, fmt.Errorf("ipmi: no response after retries: %v", lastErr)
}

// rawCall executes an IPMI request over the authenticated session and
// returns the completion code and response data. netFn is the request
// netFn (rsp will be netFn+1).
func (s *Session) rawCall(netFn, cmd byte, data []byte) (byte, []byte, error) {
	s.rqSeq = (s.rqSeq + 1) & 0x3F
	lanReq := buildLANMessage(netFn, s.rqSeq, cmd, data)

	// AES-CBC-128 encrypted, HMAC-SHA1-96 integrity, IPMI payload type.
	// Outbound session seq increments per message.
	seq := s.outSeq
	s.outSeq++
	iv := newIV()
	resp, err := s.roundtrip(payloadIPMI, s.sessionID, seq, lanReq, s.k1, s.k2, iv)
	if err != nil {
		return 0, nil, err
	}
	_, _, cc, out, err := parseLANResponse(resp)
	if err != nil {
		return 0, nil, err
	}
	return cc, out, nil
}

// rakpStatusName maps IPMI 2.0 §13.24 Table 13-16 RAKP status codes to
// human-readable strings so failures point at the actual problem.
func rakpStatusName(st byte) string {
	switch st {
	case 0x00:
		return "no errors"
	case 0x01:
		return "insufficient resources for session"
	case 0x02:
		return "invalid session ID"
	case 0x03:
		return "invalid payload type"
	case 0x04:
		return "invalid authentication algorithm"
	case 0x05:
		return "invalid integrity algorithm"
	case 0x06:
		return "no matching authentication payload"
	case 0x07:
		return "no matching integrity payload"
	case 0x08:
		return "inactive session ID"
	case 0x09:
		return "invalid role"
	case 0x0A:
		return "unauthorized role or privilege level requested"
	case 0x0B:
		return "insufficient resources to create a session at the requested role"
	case 0x0C:
		return "invalid name length"
	case 0x0D:
		return "unauthorized name"
	case 0x0E:
		return "unauthorized GUID"
	case 0x0F:
		return "invalid integrity check value"
	case 0x10:
		return "invalid confidentiality algorithm"
	case 0x11:
		return "no cipher suite match with proposed security algorithms"
	case 0x12:
		return "illegal or unrecognized parameter"
	default:
		return fmt.Sprintf("unknown status 0x%02x", st)
	}
}
