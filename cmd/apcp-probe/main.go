// Command apcp-probe dials the BMC, completes the APCP/AVMP handshake, and
// prints every frame the BMC pushes back. It is Phase 1's acceptance
// vehicle: verifies TLS, auth, heartbeat, and per-frame identification.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/zheng1/beefkvm/internal/apcp"
	"github.com/zheng1/beefkvm/internal/tlsdial"
)

func main() {
	var (
		host       = flag.String("host", envOr("BMC_HOST", ""), "BMC host")
		port       = flag.Int("port", 2068, "BMC port")
		user       = flag.String("user", envOr("BMC_USER", "admin"), "BMC user")
		pass       = flag.String("pass", envOr("BMC_PASS", ""), "BMC password (env BMC_PASS preferred)")
		pin        = flag.String("pin", envOr("BMC_PIN", ""), "expected BMC cert SHA-256 (hex); required for KVM channel; use --learn-pin to bootstrap")
		learnPin   = flag.Bool("learn-pin", false, "print the BMC's cert fingerprint and exit")
		holdSecs   = flag.Int("hold", 60, "seconds to stay connected after login")
		preempt    = flag.Bool("preempt", false, "preempt any existing session")
		channelStr = flag.String("type", "kvm", "channel: kvm (SSL) or vm (plain)")
	)
	flag.Parse()
	if *pass == "" {
		log.Fatal("apcp-probe: --pass or BMC_PASS required")
	}
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))

	var chanType apcp.SessionType
	switch *channelStr {
	case "kvm":
		chanType = apcp.SessionTypeKVM
	case "vm":
		chanType = apcp.SessionTypeVM
	default:
		log.Fatalf("apcp-probe: unknown --type %q; want kvm or vm", *channelStr)
	}

	if *learnPin {
		if err := runLearnPin(addr, *host); err != nil {
			log.Fatalf("apcp-probe: learn-pin: %v", err)
		}
		return
	}

	if chanType == apcp.SessionTypeKVM && *pin == "" {
		log.Fatal("apcp-probe: --pin required for KVM channel; run with --learn-pin first")
	}

	sess := &apcp.Session{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 10*time.Second)
		},
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) {
			return tlsdial.UpgradeToTLS(raw, *host, *pin)
		},
		Type:     chanType,
		Username: *user,
		Password: *pass,
		Preempt:  *preempt,
	}
	if err := sess.Open(); err != nil {
		log.Fatalf("apcp-probe: %v", err)
	}
	defer sess.Conn.Close()
	log.Printf("apcp-probe: logged in as %s (server v%d.%d, tcp port %d, caps 0x%x)",
		sess.LoginStatus.Username,
		sess.Setup.VersionMajor, sess.Setup.VersionMinor,
		sess.Setup.TCPPort, sess.Setup.Capabilities)

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; cancel() }()

	var writeMu sync.Mutex
	stop := make(chan struct{})
	hbDone := apcp.StartHeartbeat(sess, &writeMu, stop)

	// If this is a KVM channel, kick off video stream so we see real frames.
	if chanType == apcp.SessionTypeKVM {
		writeMu.Lock()
		enableVideo := apcp.AVPTFrame{ID: 782, Payload: []byte{1, 1, 0, 0, 0, 0, 0, 0}}.Encode()
		setRes := apcp.AVPTFrame{ID: 770, Payload: []byte{4, 0, 3, 0, 0, 0, 0, 0}}.Encode() // 1024x768 big-endian shorts
		reqFrame := apcp.AVPTFrame{ID: 772, Payload: make([]byte, 8)}.Encode()
		_, _ = sess.Conn.Write(enableVideo)
		_, _ = sess.Conn.Write(setRes)
		_, _ = sess.Conn.Write(reqFrame)
		writeMu.Unlock()
		log.Printf("→ sent video-enable + set-resolution 1024x768 + initial-frame")
	}

	// Frame reader. KVM uses BEEF; VM uses AVMP.
	go func() {
		for {
			if chanType == apcp.SessionTypeVM {
				f, err := apcp.ReadFrame(sess.Conn)
				if err != nil {
					if err == io.EOF {
						log.Printf("apcp-probe: server closed stream")
					} else {
						log.Printf("apcp-probe: read: %v", err)
					}
					cancel()
					return
				}
				log.Printf("← AVMP %s (0x%04x) len=%d prefix=%x",
					apcp.MsgTypeName(f.Type), f.Type, len(f.Payload), preview(f.Payload, 32))
			} else {
				f, err := apcp.ReadAVPTFrame(sess.Conn)
				if err != nil {
					if err == io.EOF {
						log.Printf("apcp-probe: server closed stream")
					} else {
						log.Printf("apcp-probe: read: %v", err)
					}
					cancel()
					return
				}
				log.Printf("← BEEF id=%d (0x%04x) len=%d prefix=%x",
					f.ID, f.ID, len(f.Payload), preview(f.Payload, 32))
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(*holdSecs) * time.Second):
		log.Printf("apcp-probe: hold expired after %ds, disconnecting", *holdSecs)
	case err := <-hbDone:
		log.Printf("apcp-probe: heartbeat exited: %v", err)
	}
	close(stop)
}

func runLearnPin(addr, host string) error {
	raw, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()

	// SessionRequest first — the BMC only exposes TLS after that.
	var nonce [32]byte
	// deterministic bootstrap nonce; content does not matter for pin discovery
	for i := range nonce {
		nonce[i] = byte(i)
	}
	req := apcp.SessionRequest{
		SessionType: byte(apcp.SessionTypeKVM),
		Field2:      2,
		Field3:      34,
		Capability:  5,
		Nonce:       nonce,
		NonceLen:    32,
	}
	if _, err := raw.Write(apcp.EncodeSessionRequest(req)); err != nil {
		return fmt.Errorf("send SessionRequest: %w", err)
	}
	buf := make([]byte, apcp.SessionRequestLen)
	if _, err := io.ReadFull(raw, buf); err != nil {
		return fmt.Errorf("read SessionSetup: %w", err)
	}
	setup, err := apcp.DecodeSessionSetup(buf)
	if err != nil {
		return err
	}
	if !setup.SupportsSSL() {
		return fmt.Errorf("BMC does not advertise SSL (caps 0x%x)", setup.Capabilities)
	}
	fp, err := tlsdial.PinFromServer(raw, host)
	if err != nil {
		return err
	}
	fmt.Printf("BMC leaf certificate SHA-256 fingerprint:\n  %s\n\nSave it and pass with --pin or BMC_PIN.\n", fp)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func preview(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
