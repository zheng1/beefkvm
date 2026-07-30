package apcp

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

// SessionType selects which BMC channel this session opens. The BMC responds
// with different capabilities to each. Values are derived from
// com.avocent.vm.VirtualMedia (sends 2) and com.avocent.kvm.b.l (sends 3 or 4).
type SessionType byte

const (
	SessionTypeVM              SessionType = 2 // Virtual Media; BMC advertises caps=1 (plain)
	SessionTypeKVM             SessionType = 3 // KVM video/keyboard/mouse; BMC advertises caps=4 (SSL)
	SessionTypeKVMVMSubchannel SessionType = 4 // KVM's own VM subchannel; SSL
)

// Session drives the client half of the APCP handshake and holds the
// resulting authenticated stream. It never dials TCP or upgrades TLS itself;
// callers supply hooks. That keeps the state machine testable and lets
// apcp-probe and apcp-mitm reuse it identically.
type Session struct {
	// Dial returns a fresh TCP connection to the BMC. Called once.
	Dial func() (net.Conn, error)
	// UpgradeTLS wraps a raw connection with TLS. Called only if the
	// server SessionSetup indicates SSL support. It must return a Conn that
	// transparently encrypts subsequent I/O over the same underlying socket.
	UpgradeTLS func(raw net.Conn) (net.Conn, error)
	// Type is the channel to open. Defaults to KVM.
	Type SessionType
	// SkipLogin makes Open return immediately after any TLS upgrade, without
	// sending an AVMP/AVPT login. Used for the KVM video companion socket
	// (SessionTypeKVMVMSubchannel) which authenticates via a dc sync packet
	// instead.
	SkipLogin bool
	// Username and Password are sent in the AVMP Login frame after any TLS
	// upgrade. Password is transmitted plaintext under TLS, matching the
	// Java client's behaviour (see VirtualMedia.login).
	Username, Password string
	// Preempt kicks any existing session on the same channel if true.
	Preempt bool

	// State visible after Open returns.
	Conn             net.Conn // authenticated stream, use for Reads/Writes
	Setup            SessionSetup
	LoginStatus      LoginStatus      // VM channel
	KVMLoginResponse KVMLoginResponse // KVM channel
	ClientRandom     [32]byte
	KVMSessionID     uint32 // client-generated (`z` in Java)
}

// Open runs the full APCP → optional TLS → AVMP Login sequence. It writes
// nothing back if any step fails.
func (s *Session) Open() error {
	raw, err := s.Dial()
	if err != nil {
		return fmt.Errorf("apcp.Session: dial: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = raw.Close()
		}
	}()

	if _, err := rand.Read(s.ClientRandom[:]); err != nil {
		return fmt.Errorf("apcp.Session: rand: %w", err)
	}
	if s.Type == 0 {
		s.Type = SessionTypeKVM
	}
	// The KVM client encodes Field2/Field3 as (2, 34); the VM client
	// leaves both zero. We haven't found a functional difference, so mirror
	// each channel's Java client verbatim.
	var f2, f3 byte
	if s.Type == SessionTypeKVM || s.Type == SessionTypeKVMVMSubchannel {
		f2, f3 = 2, 34
	}
	req := SessionRequest{
		SessionType: byte(s.Type),
		Field2:      f2,
		Field3:      f3,
		Capability:  5,
		Nonce:       s.ClientRandom,
		NonceLen:    32,
	}
	if _, err := raw.Write(EncodeSessionRequest(req)); err != nil {
		return fmt.Errorf("apcp.Session: send SessionRequest: %w", err)
	}

	setupBuf := make([]byte, SessionRequestLen)
	if _, err := io.ReadFull(raw, setupBuf); err != nil {
		return fmt.Errorf("apcp.Session: read SessionSetup: %w", err)
	}
	setup, err := DecodeSessionSetup(setupBuf)
	if err != nil {
		return err
	}
	s.Setup = setup

	conn := raw
	if setup.SupportsSSL() {
		if s.UpgradeTLS == nil {
			return errors.New("apcp.Session: server requires SSL but no UpgradeTLS hook set")
		}
		tls, err := s.UpgradeTLS(raw)
		if err != nil {
			return fmt.Errorf("apcp.Session: tls: %w", err)
		}
		conn = tls
	}

	if s.SkipLogin {
		s.Conn = conn
		success = true
		return nil
	}

	login, err := s.buildLogin()
	if err != nil {
		return err
	}
	if _, err := conn.Write(login); err != nil {
		return fmt.Errorf("apcp.Session: send Login: %w", err)
	}

	if s.Type == SessionTypeKVM || s.Type == SessionTypeKVMVMSubchannel {
		// KVM channel: response is a BEEF frame. Read login response so
		// the caller can inspect flags (e.g. companion-socket bit) and
		// pick up server-assigned session id A.
		lr, err := ReadAVPTFrame(conn)
		if err != nil {
			return fmt.Errorf("apcp.Session: read KVM login response: %w", err)
		}
		s.KVMLoginResponse = DecodeKVMLoginResponse(lr)
		s.Conn = conn
		success = true
		return nil
	}

	// LoginStatus is a fixed 111-byte AVMP frame (VM channel only).
	statusBuf := make([]byte, 111)
	if _, err := io.ReadFull(conn, statusBuf); err != nil {
		return fmt.Errorf("apcp.Session: read LoginStatus: %w", err)
	}
	ls, err := DecodeLoginStatus(statusBuf)
	if err != nil {
		return err
	}
	s.LoginStatus = ls
	if ls.Status != 0 {
		return fmt.Errorf("apcp.Session: login failed: %s", LoginStatusName(ls.Status))
	}

	s.Conn = conn
	success = true
	return nil
}

// buildLogin picks the right login frame for the channel type.
func (s *Session) buildLogin() ([]byte, error) {
	if s.Type == SessionTypeVM {
		return EncodeLogin(LoginRequest{
			Username: s.Username, Password: s.Password, Preempt: s.Preempt,
		})
	}
	// KVM channel needs a random 24-bit session ID (Java uses
	// (int)(Math.random() * 1e7)).
	nMax, _ := rand.Int(rand.Reader, big.NewInt(10_000_000))
	sessID := uint32(nMax.Int64())
	s.KVMSessionID = sessID
	return EncodeKVMLogin(KVMLoginRequest{
		Username:   s.Username,
		Password:   s.Password,
		ProtoMajor: 1, // matches Java client wire capture (protocol version 1.0)
		ProtoMinor: 0,
		Session:    sessID,
		Extended:   s.Setup.VersionMajor >= 2,
		Option:     0,
	})
}

// StartHeartbeat launches a goroutine that sends the appropriate heartbeat
// frame for the given session channel at 9.5-second cadence until stop is
// closed or a write fails. AVMP heartbeats are used for the VM channel;
// BEEF/AVPT id=1024 heartbeats are used for the KVM channel.
func StartHeartbeat(sess *Session, writeLock interface {
	Lock()
	Unlock()
}, stop <-chan struct{}) <-chan error {
	done := make(chan error, 1)
	var hb []byte
	if sess.Type == SessionTypeVM {
		hb = Heartbeat().Encode()
	} else {
		hb = KVMHeartbeat()
	}
	go func() {
		t := time.NewTicker(time.Duration(HeartbeatIntervalMs-500) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-t.C:
				writeLock.Lock()
				_, err := sess.Conn.Write(hb)
				writeLock.Unlock()
				if err != nil {
					done <- err
					return
				}
			}
		}
	}()
	return done
}

// PeekFrameHeader inspects the first bytes of a stream without consuming them
// beyond the header. Useful in the MITM which needs to distinguish AVMP data
// from raw APCP setup traffic.
func PeekFrameHeader(hdr []byte) (magic [4]byte, length uint32, msgType uint16, ok bool) {
	if len(hdr) < AVMPHeaderLen {
		return
	}
	copy(magic[:], hdr[0:4])
	length = binary.BigEndian.Uint32(hdr[4:8])
	msgType = binary.BigEndian.Uint16(hdr[8:10])
	ok = true
	return
}
