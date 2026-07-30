// Command beefkvm serves a browser-based KVM and management console for
// Avocent-style BMCs. It:
//
//   - Opens the KVM session via internal/apcp (APCP/AVPT, the "BEEF" framing)
//   - Decodes the custom DCT video codec via internal/video
//   - Streams framebuffer diffs over WebSocket to a browser canvas
//   - Accepts keyboard/mouse events and forwards them as USB HID reports
//   - Bridges IPMI 2.0 over LAN for power, sensors, SEL, users, and SoL
//   - Serves a single-page frontend from an embedded fs
//
// Everything is pure Go: no Java client, no browser plugin, no vendor binary.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/zheng1/beefkvm/internal/apcp"
	"github.com/zheng1/beefkvm/internal/input"
	"github.com/zheng1/beefkvm/internal/tlsdial"
	"github.com/zheng1/beefkvm/internal/vcard"
	"github.com/zheng1/beefkvm/internal/video"
	"github.com/zheng1/beefkvm/internal/vm"
)

//go:embed web
var webFS embed.FS

func main() {
	var (
		host     = flag.String("host", envOr("BMC_HOST", ""), "BMC host")
		port     = flag.Int("port", 2068, "BMC port")
		user     = flag.String("user", envOr("BMC_USER", "admin"), "BMC user")
		pass     = flag.String("pass", envOr("BMC_PASS", ""), "BMC password")
		pin      = flag.String("pin", envOr("BMC_PIN", ""), "BMC cert fingerprint")
		listen   = flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
		videoRes = flag.String("res", "1024x768", "initial video resolution")
		iso      = flag.String("iso", "", "optional ISO to mount via KVM-VM subchannel (SessionType=4)")

		// IPMI over LAN (UDP 623) — separate credentials from the APCP
		// KVM/VM channel. Defaults reuse the BMC user/pass which usually
		// have IPMI privileges too. Empty user disables sensors + SEL.
		ipmiUser = flag.String("ipmi-user", envOr("BMC_IPMI_USER", envOr("BMC_USER", "")), "IPMI user (empty disables /api/sensors + /api/sel)")
		ipmiPass = flag.String("ipmi-pass", envOr("BMC_IPMI_PASS", envOr("BMC_PASS", "")), "IPMI password")
	)
	flag.Parse()
	if *host == "" {
		log.Fatal("beefkvm: --host (or BMC_HOST) is required, e.g. --host bmc.example or an IP")
	}
	if *pass == "" || *pin == "" {
		log.Fatal("beefkvm: --pass and --pin (or BMC_PASS/BMC_PIN) required; " +
			"learn the pin with: apcp-probe --host <bmc> --learn-pin")
	}

	w, h := 1024, 768
	fmt.Sscanf(*videoRes, "%dx%d", &w, &h)

	bmcAddr := net.JoinHostPort(*host, strconv.Itoa(*port))

	// One-shot random session token: required on every /ws upgrade and on
	// / requests as a cookie. Printed once to the operator terminal so
	// only they can reach the service — this is a single-user local tool.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Fatalf("beefkvm: rand: %v", err)
	}
	token := hex.EncodeToString(tokenBytes)

	bridge := &bridge{
		bmcAddr: bmcAddr,
		host:    *host,
		user:    *user,
		pass:    *pass,
		pin:     *pin,
		fb:      video.NewFramebuffer(w, h),
		dec15:   video.NewDecoder(video.Depth15),
		dec21:   video.NewDecoder(video.Depth21),
		dec7g:   video.NewDecoder(video.Depth7Gray),
		dec7p:   video.NewDecoder(video.Depth7Palette),
		decDCT:  video.NewJPEGDecoder(),
		decAvo:  video.NewAvocentDecoder(),
		decAvo2: video.NewAvocentV2(),
		initW:   w, initH: h,
		token:  token,
		mounts: make(map[uint16]*mount),
	}
	if err := bridge.connectBMC(); err != nil {
		log.Fatalf("beefkvm: BMC connect: %v", err)
	}
	// pushLoop feeds the framebuffer to browser WebSockets; start it once so it
	// survives BMC reconnects. superviseBMC re-establishes a dropped session.
	go bridge.pushLoop()
	go bridge.superviseBMC()

	// If --iso given, open a KVM-VM subchannel (SessionType=4) to mount
	// the ISO alongside the live KVM session.
	if *iso != "" {
		if err := bridge.mountISOSubchannel(*iso); err != nil {
			log.Fatalf("beefkvm: mount ISO via subchannel: %v", err)
		}
		log.Printf("beefkvm: ISO mounted via KVM-VM subchannel: %s", *iso)
	}

	// If IPMI credentials are supplied, spin up the RMCP+ client for
	// sensor + SEL (and Track B power once it lands). Session is opened
	// lazily on first request so an unreachable BMC UDP port doesn't
	// block beefkvm startup.
	if *ipmiUser != "" && *ipmiPass != "" {
		bridge.ipmi = newIPMI(*host, *ipmiUser, *ipmiPass)
	}

	sub, _ := fs.Sub(webFS, "web")
	// no-store on static assets so browsers never serve a stale index.html
	// from a prior build (which would run old decode/keymap JS).
	fileSrv := http.FileServer(http.FS(sub))
	http.Handle("/", bridge.gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		fileSrv.ServeHTTP(w, r)
	})))
	http.HandleFunc("/ws", bridge.serveWS)
	http.HandleFunc("/ws/sol", bridge.serveSOL)
	http.Handle("/api/screen.png", bridge.gate(http.HandlerFunc(bridge.handleScreenPNG)))
	http.Handle("/api/debug/drop", bridge.gate(http.HandlerFunc(bridge.handleDebugDrop)))
	http.Handle("/api/cert", bridge.gate(http.HandlerFunc(bridge.handleCert)))
	http.Handle("/api/smartcard", bridge.gate(http.HandlerFunc(bridge.handleSmartcard)))
	http.Handle("/api/vm/mount", bridge.gate(http.HandlerFunc(bridge.handleVMMount)))
	http.Handle("/api/vm/unmount", bridge.gate(http.HandlerFunc(bridge.handleVMUnmount)))
	http.Handle("/api/vm/status", bridge.gate(http.HandlerFunc(bridge.handleVMStatus)))
	http.Handle("/api/vm/upload", bridge.gate(http.HandlerFunc(bridge.handleVMUpload)))
	bridge.registerIPMIHandlers(http.DefaultServeMux)

	log.Printf("beefkvm: listening on http://%s?t=%s", *listen, token)
	log.Printf("beefkvm: (open the URL above; the token cookie is set on first visit)")
	log.Printf("beefkvm: BMC session live at %s res=%dx%d", bmcAddr, w, h)
	if err := http.ListenAndServe(*listen, nil); err != nil {
		log.Fatal(err)
	}
}

type bridge struct {
	bmcAddr, host, user, pass, pin string
	initW, initH                   int
	token                          string // required for all HTTP + WS reqs

	sess    *apcp.Session
	fb      *video.Framebuffer
	dec15   *video.Decoder
	dec21   *video.Decoder
	dec7g   *video.Decoder
	dec7p   *video.Decoder
	decDCT  *video.JPEGDecoder
	decAvo  *video.AvocentDecoder
	decAvo2 *video.AvocentV2
	// avoAgg accumulates multi-fragment ib packets: the first fragment
	// (byte[8]&1 == 1) starts a new buffer; subsequent fragments with
	// byte[8]&1 == 0 append only their body (bytes 12..end) onto avoAgg.
	// Decoded when the next fragment-start arrives.
	avoAgg []byte

	writeMu   sync.Mutex // guards writes to sess.Conn
	vidMu     sync.Mutex // guards writes to the video companion socket
	fbMu      sync.Mutex // guards b.fb pixels/state between decode and push/snapshot
	sinkMu    sync.Mutex // guards subscribers
	videoConn net.Conn   // the socket video frames arrive on (companion, or primary)
	subs      []*subscriber

	// sessDead is closed once when the current BMC session's video/control/
	// heartbeat loop exits; superviseBMC waits on it to trigger a reconnect.
	// hbStop signals the current session's heartbeat goroutine to end so a
	// stale heartbeat can't outlive its socket across a reconnect.
	sessDead chan struct{}
	hbStop   chan struct{}

	mountsMu sync.Mutex
	mounts   map[uint16]*mount

	// ipmi is the RMCP+ session shared by sensor + SEL (Track C) and, when
	// it lands, chassis control (Track B). nil when the operator hasn't
	// supplied an IPMI user (--ipmi-user or BMC_IPMI_USER).
	ipmi *ipmiState
}

// mount tracks a single active virtual-media attachment over a KVM-VM
// subchannel. tempPath is set for uploaded files that should be deleted
// when the mount goes away.
type mount struct {
	driveID   uint16
	media     *vm.Media
	conn      net.Conn
	sess      *vm.Session
	tempPath  string
	mountedAt time.Time
}

var debugCountRaw int

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// gate wraps an http.Handler with token auth. Accepts either `t=<token>`
// query string or the `ikvm-token` cookie. Rejects with 401 otherwise.
func (b *bridge) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.checkToken(r) {
			http.SetCookie(w, &http.Cookie{
				Name:     "ikvm-token",
				Value:    b.token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "missing or bad token", http.StatusUnauthorized)
	})
}

// checkToken verifies the request presents the session token via query or
// cookie. Constant-time compare.
func (b *bridge) checkToken(r *http.Request) bool {
	if q := r.URL.Query().Get("t"); q != "" &&
		subtle.ConstantTimeCompare([]byte(q), []byte(b.token)) == 1 {
		return true
	}
	if c, err := r.Cookie("ikvm-token"); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(b.token)) == 1 {
		return true
	}
	return false
}

// sameOrigin returns true when the WebSocket Origin header matches the
// listening host, blocking cross-site WebSocket hijacking.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

type subscriber struct {
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
}

func (b *bridge) connectBMC() error {
	sess := &apcp.Session{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", b.bmcAddr, 10*time.Second)
		},
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) {
			return tlsdial.UpgradeToTLS(raw, b.host, b.pin)
		},
		Type:     apcp.SessionTypeKVM,
		Username: b.user,
		Password: b.pass,
		Preempt:  true,
	}
	if err := sess.Open(); err != nil {
		return err
	}
	b.sess = sess
	log.Printf("bridge: KVM session ready (server v%d.%d) status=%d flags=0x%04x companion=%v",
		sess.Setup.VersionMajor, sess.Setup.VersionMinor,
		sess.KVMLoginResponse.Status, sess.KVMLoginResponse.Flags,
		sess.KVMLoginResponse.NeedsCompanion)

	// Per-session death signal: closed exactly once when the video, control,
	// or heartbeat loop exits. superviseBMC blocks on it to reconnect.
	dead := make(chan struct{})
	var deadOnce sync.Once
	markDead := func() { deadOnce.Do(func() { close(dead) }) }
	b.sessDead = dead

	// Start heartbeat and video stream on the primary (input/control) socket.
	stop := make(chan struct{})
	b.hbStop = stop
	hbDone := apcp.StartHeartbeat(sess, &b.writeMu, stop)
	go func() {
		if err := <-hbDone; err != nil {
			log.Printf("bridge: PRIMARY heartbeat stopped: %v", err)
		}
		markDead()
	}()

	// If the server requires a companion socket for video, open one now.
	// Server-assigned session ID is in KVMLoginResponse.SessionID and our
	// client-random ID in sess.KVMSessionID.
	videoConn := sess.Conn
	if sess.KVMLoginResponse.NeedsCompanion {
		vc, err := b.openVideoCompanion(sess.KVMSessionID, sess.KVMLoginResponse.SessionID)
		if err != nil {
			return fmt.Errorf("open video companion: %w", err)
		}
		videoConn = vc
		log.Printf("bridge: video companion socket open")
	}
	b.videoConn = videoConn

	b.writeMu.Lock()
	_, _ = sess.Conn.Write(apcp.AVPTFrame{ID: 782, Payload: []byte{1, 1, 0, 0, 0, 0, 0, 0}}.Encode())
	res := make([]byte, 8)
	binary.BigEndian.PutUint16(res[0:2], uint16(b.initW))
	binary.BigEndian.PutUint16(res[2:4], uint16(b.initH))
	_, _ = sess.Conn.Write(apcp.AVPTFrame{ID: 770, Payload: res}.Encode())
	_, _ = sess.Conn.Write(apcp.AVPTFrame{ID: 772, Payload: make([]byte, 8)}.Encode())
	b.writeMu.Unlock()
	log.Printf("bridge: sent video-enable + set-res %dx%d + initial-frame", b.initW, b.initH)

	// Install a JPEG debug printer so we see what the decoder outputs.
	debugLimit := 3
	debugCount := 0
	video.SetJPEGDebug(func(f string, args ...any) {
		if debugCount < debugLimit {
			debugCount++
			log.Printf("[jpeg] "+f, args...)
		}
	})

	// Frame reader on the video socket (companion or primary). When it exits
	// (EOF/error) the session is dead → signal the supervisor to reconnect.
	go func() {
		b.readVideoLoop(videoConn)
		markDead()
	}()
	// Frame reader on the control socket if it differs (drains device status etc).
	if videoConn != sess.Conn {
		go func() {
			b.readControlLoop(sess.Conn)
			markDead()
		}()
	}
	// pushLoop is started once from main so it survives reconnects.
	// Video is kept flowing by the per-frame ACK sent from readVideoLoop
	// (frame id=0), not by polling — this BMC's stream is request/response.
	return nil
}

// handleDebugDrop forcibly closes the current BMC session's sockets to simulate
// a mid-session drop, exercising the reconnect supervisor. Token-gated. The
// read loops error out → markDead → superviseBMC tears down and reconnects.
func (b *bridge) handleDebugDrop(w http.ResponseWriter, r *http.Request) {
	primary := b.sess
	vc := b.videoConn
	if vc != nil {
		vc.Close()
	}
	if primary != nil && primary.Conn != nil && primary.Conn != vc {
		primary.Conn.Close()
	}
	log.Printf("bridge: DEBUG drop requested — closed BMC sockets")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"dropped":true}`))
}

// superviseBMC keeps the BMC session alive across drops. It blocks on the
// current session's death signal, tears the dead session down, then reconnects
// with exponential backoff. Runs for the process lifetime; started once from
// main after the initial connect succeeds.
func (b *bridge) superviseBMC() {
	for {
		<-b.sessDead
		log.Printf("bridge: BMC session lost — reconnecting")
		b.teardownSession()
		backoff := time.Second
		for {
			time.Sleep(backoff)
			if err := b.connectBMC(); err != nil {
				log.Printf("bridge: reconnect failed: %v (retry in %s)", err, backoff)
				if backoff < 15*time.Second {
					backoff *= 2
				}
				continue
			}
			log.Printf("bridge: BMC session re-established")
			break
		}
	}
}

// teardownSession closes the dead session's sockets and stops its heartbeat so
// nothing from the old session leaks into the new one. b.sess is left pointing
// at the (now-closed) old session until connectBMC swaps it — input writes to a
// closed conn just error harmlessly, avoiding a nil-deref window.
func (b *bridge) teardownSession() {
	if b.hbStop != nil {
		close(b.hbStop)
		b.hbStop = nil
	}
	var primary net.Conn
	if b.sess != nil {
		primary = b.sess.Conn
	}
	if b.videoConn != nil && b.videoConn != primary {
		b.videoConn.Close()
	}
	if primary != nil {
		primary.Close()
	}
	// Drop any half-accumulated multi-fragment tile so the fresh stream can't
	// be corrupted by leftover bytes from the old one.
	b.fbMu.Lock()
	b.avoAgg = nil
	b.fbMu.Unlock()
}

// openVideoCompanion opens a second TCP+TLS connection to the BMC with
// SessionType=4 and sends the dc sync packet. Returns the authenticated
// video-only conn. Companion path skips AVPT login — the dc packet is the
// only identification the server needs.
func (b *bridge) openVideoCompanion(clientSessID, serverSessID uint32) (net.Conn, error) {
	sess := &apcp.Session{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", b.bmcAddr, 10*time.Second)
		},
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) {
			return tlsdial.UpgradeToTLS(raw, b.host, b.pin)
		},
		Type:      apcp.SessionTypeKVMVMSubchannel,
		SkipLogin: true,
	}
	if err := sess.Open(); err != nil {
		return nil, err
	}
	sync := apcp.EncodeVideoSyncDC(clientSessID, serverSessID)
	if _, err := sess.Conn.Write(sync); err != nil {
		sess.Conn.Close()
		return nil, fmt.Errorf("write dc: %w", err)
	}
	// The companion is a full-fledged socket that the BMC idle-times-out
	// (~31s) if it goes quiet after the initial screen burst. Closing it
	// cascades into the primary session teardown, which breaks input. Keep
	// it alive with the same KVM id=1024 heartbeat the primary uses.
	hbStop := make(chan struct{})
	hbDone := apcp.StartHeartbeat(sess, &b.vidMu, hbStop)
	go func() {
		if err := <-hbDone; err != nil {
			log.Printf("bridge: COMPANION heartbeat stopped: %v", err)
		}
	}()
	return sess.Conn, nil
}

// mountISOSubchannel is a convenience wrapper: opens an ISO from disk and
// hands it to mountMediaSubchannel.
func (b *bridge) mountISOSubchannel(isoPath string) error {
	media, err := vm.OpenCD(isoPath)
	if err != nil {
		return fmt.Errorf("open ISO: %w", err)
	}
	if _, err := b.mountMediaSubchannel(media); err != nil {
		media.Close()
		return err
	}
	return nil
}

// mountMediaSubchannel opens a KVM-VM subchannel (SessionType=4) alongside
// the active KVM session and runs the AVMP protocol over it to attach the
// given media. Unlike a standalone SessionType=2, this rides on the same
// KVM authentication (via dc sync), so it doesn't count as a second login.
// Returns the mount handle for later status/unmount calls.
func (b *bridge) mountMediaSubchannel(media *vm.Media) (*mount, error) {
	if b.sess == nil {
		return nil, fmt.Errorf("KVM session not open")
	}

	b.mountsMu.Lock()
	if _, exists := b.mounts[0]; exists {
		b.mountsMu.Unlock()
		return nil, fmt.Errorf("drive_id=0 already mounted; unmount first")
	}
	b.mountsMu.Unlock()

	subConn, err := b.openVMSession()
	if err != nil {
		return nil, fmt.Errorf("open VM session: %w", err)
	}

	var subMu sync.Mutex
	// Keep the VM session alive with its own heartbeat, like the CLI does.
	// Without it the BMC tears the VM session down on the idle timeout.
	vmStop := make(chan struct{})
	apcp.StartHeartbeat(&apcp.Session{Conn: subConn, Type: apcp.SessionTypeVM}, &subMu, vmStop)
	vmSess := &vm.Session{
		Conn:    subConn,
		WriteMu: &subMu,
		Media:   media,
	}
	vmSess.SetLogger(log.New(os.Stderr, "[subchannel-vm] ", log.LstdFlags))

	// Attach smart-card (CAC/PIV) redirection if a local PC/SC reader exists, so
	// the target can authenticate against an inserted card over this VM session.
	// Best-effort: with no reader, VCard stays nil and nothing changes.
	var scReader vcard.Reader
	if reader, err := vcard.OpenReader(); err != nil {
		log.Printf("bridge: smart-card redirection off: %v", err)
	} else {
		scReader = reader
		vc := vcard.NewSession(reader)
		vc.SetLogger(log.New(os.Stderr, "[vcard] ", log.LstdFlags))
		vmSess.VCard = vc
		log.Printf("bridge: smart-card redirection ON (reader: %s, card present: %v)", reader.Name(), reader.Present())
	}

	m := &mount{
		driveID:   0,
		media:     media,
		conn:      subConn,
		sess:      vmSess,
		mountedAt: time.Now(),
	}
	b.mountsMu.Lock()
	b.mounts[0] = m
	b.mountsMu.Unlock()

	go func() {
		if err := vmSess.Run(); err != nil {
			log.Printf("bridge: KVM-VM session ended: %v", err)
		}
		close(vmStop)
		// Session ended (BMC released / socket closed). Drop the record so
		// the UI reflects reality.
		b.mountsMu.Lock()
		if cur, ok := b.mounts[m.driveID]; ok && cur == m {
			delete(b.mounts, m.driveID)
		}
		b.mountsMu.Unlock()
		if scReader != nil {
			scReader.Close()
		}
		subConn.Close()
		media.Close()
		if m.tempPath != "" {
			// tempPath is a directory created by handleVMUpload; remove it
			// and all uploaded contents.
			os.RemoveAll(m.tempPath)
		}
	}()
	return m, nil
}

// openVMSession opens a full standalone SessionType=2 (Virtual Media) session
// with its own username/password login. This is what actually works against
// this BMC: the KVM-VM subchannel (Type=4 + video dc-sync) is rejected by the
// firmware ("connection reset by peer") for VM traffic, but a first-class VM
// session is accepted exactly like the ikvm-vm CLI. The BMC allows this
// alongside the live KVM video session.
func (b *bridge) openVMSession() (net.Conn, error) {
	sess := &apcp.Session{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", b.bmcAddr, 10*time.Second)
		},
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) {
			return tlsdial.UpgradeToTLS(raw, b.host, b.pin)
		},
		Type:     apcp.SessionTypeVM,
		Username: b.user,
		Password: b.pass,
	}
	if err := sess.Open(); err != nil {
		return nil, err
	}
	return sess.Conn, nil
}

// readVideoLoop consumes AVPT frames from the video socket, decoding tile
// packets into the framebuffer. After each frame it sends a video-ACK
// (frame id=0 with a rolling counter) — this BMC's video stream is
// request/response: without the per-frame ACK the server sends the initial
// burst then freezes. Verified against the Avocent Java client: m.java calls
// s.y() after processing each frame, which sends the `eb` packet (super(0),
// payload[0]=counter) on the video socket.
func (b *bridge) readVideoLoop(conn net.Conn) {
	nFrames := 0
	var ackCounter byte
	ack := func() {
		ackCounter++
		frame := apcp.AVPTFrame{ID: 0, Payload: []byte{ackCounter, 0, 0, 0, 0, 0, 0, 0}}.Encode()
		b.vidMu.Lock()
		conn.Write(frame)
		b.vidMu.Unlock()
	}
	for {
		f, err := apcp.ReadAVPTFrame(conn)
		if err != nil {
			if err == io.EOF {
				log.Printf("bridge: video socket closed after %d frames", nFrames)
				return
			}
			log.Printf("bridge: video read err: %v", err)
			return
		}
		nFrames++
		if nFrames <= 20 || nFrames%200 == 0 {
			log.Printf("← video frame #%d id=%d len=%d", nFrames, f.ID, len(f.Payload))
		}
		b.handleVideoFrame(f)
		ack()
	}
}

// readControlLoop drains the primary (control/input/heartbeat) socket to
// keep it healthy — the BMC sends device-status and other admin frames.
func (b *bridge) readControlLoop(conn net.Conn) {
	for {
		f, err := apcp.ReadAVPTFrame(conn)
		if err != nil {
			log.Printf("bridge: PRIMARY control socket closed: %v", err)
			return
		}
		_ = f
	}
}

// handleVideoFrame decodes one tile-family AVPT frame into the framebuffer.
func (b *bridge) handleVideoFrame(f apcp.AVPTFrame) {
	// Frame 136: hardware cursor shape
	if f.ID == 136 {
		b.handleCursorShape(f)
		return
	}
	// All remaining paths mutate the shared framebuffer (or the decoder
	// palette feeding it). Serialize against pushLoop/serializeFB and the
	// snapshot handler, which read b.fb.Pixels concurrently.
	b.fbMu.Lock()
	defer b.fbMu.Unlock()
	// Frame 138: VGA palette update. Applies to the Depth7Palette decoder
	// (dec7p). Payload: [start_index:1][count:1][ARGB entries: count*4].
	if f.ID == 138 || f.ID == 34314 {
		b.handlePaletteUpdate(f)
		return
	}
	if !(f.ID >= 129 && f.ID <= 138 || f.ID == 34305 || f.ID == 34306 || f.ID == 34307 || f.ID == 34310 || f.ID == 34314) {
		return
	}
	if len(f.Payload) < 12 {
		return
	}
	x := binary.BigEndian.Uint16(f.Payload[4:6])
	y := binary.BigEndian.Uint16(f.Payload[6:8])
	// Java ib.setData: bytes 1..3 packed as 24-bit BE with hi12=r (x
	// tile offset in pixels), lo12=s (y tile offset in pixels). Used by
	// id-134 (JPEG-DCT) below. Non-DCT codec paths still use bytes 4..7.
	n3 := uint32(f.Payload[1])<<16 | uint32(f.Payload[2])<<8 | uint32(f.Payload[3])
	dctPixelX := int((n3 >> 12) & 0xFFF)
	dctPixelY := int(n3 & 0xFFF)
	data := f.Payload[12:]

	// id 134/34310 is the JPEG-DCT codec (com.avocent.kvm.a.a.d). The tile
	// header in the DCT stream carries subsampling + quality bytes before
	// the entropy-coded blocks; for the AVR2300 firmware observed here the
	// tile is a fixed 16x16 or 8x8 MCU, mode 0 (4:4:4), quality preset 3.
	// Concrete parameters need to be extracted from the packet; we make
	// best-effort defaults so the frame at least renders.
	if f.ID == 134 || f.ID == 34310 {
		// Payload layout after AVPT header (from ib.setData):
		//   [0]     packet subtype (o) — 0/1 = DCT, 2/3 = palette-only
		//   [1..3]  packed 24-bit tile origin (r hi12 = x, s lo12 = y)
		//   [4..5]  height (m short BE)
		//   [6..7]  width (n short BE)
		//   [8]     flag bits: k(1)|j(2)|l(4)|w(8)|x(16)
		//   [9..10] short: lumaQ(6b)|chromaQ(6b)|mode(4b)
		//   [11]    p (lo nibble), q (hi nibble)
		//   [12..]  DCT/palette entropy-coded body
		if debugCountRaw < 3 {
			debugCountRaw++
			log.Printf("[avo] tile hdr: fullpayload[0..15]=%x  bodyLen=%d",
				f.Payload[:min(16, len(f.Payload))], len(data))
		}
		if len(f.Payload) < 12 {
			return
		}
		// Multi-fragment aggregation per Java m.java line 478-490 / ib.a(ib).
		// byte[8]&1 = fragment-start (k). On start: decode any prior aggregate
		// then reset avoAgg to this whole packet. On continuation: append only
		// the body bytes (12..end) onto avoAgg.
		startFrag := (f.Payload[8] & 1) != 0
		if startFrag {
			if len(b.avoAgg) > 0 {
				b.decAvo.DecodePacket(b.fb, b.avoAgg)
			}
			b.avoAgg = append(b.avoAgg[:0], f.Payload...)
		} else {
			if len(b.avoAgg) == 0 {
				// orphan continuation without a prior start — decode alone
				b.decAvo.DecodePacket(b.fb, f.Payload)
				return
			}
			b.avoAgg = append(b.avoAgg, f.Payload[12:]...)
		}
		return
	}
	_ = dctPixelX
	_ = dctPixelY
	_ = data

	pos := int(y)*b.fb.W + int(x)
	if pos >= 0 && pos < len(b.fb.Pixels) {
		b.fb.Cur = pos
	}
	var dec *video.Decoder
	switch f.ID {
	case 130, 34306:
		dec = b.dec7p
	// 138/34314 handled above by handlePaletteUpdate
	case 131, 34307:
		dec = b.dec7g
	default:
		dec = b.dec15
	}
	if _, err := dec.Decode(b.fb, data); err != nil {
		log.Printf("bridge: decode err on id=0x%04x: %v", f.ID, err)
	}
}

// handleCursorShape parses frame 136 (hardware cursor bitmap) and broadcasts
// it to all WebSocket clients. Payload layout (from Java client observations):
//
//	[0..1]  hotspot X (int16 BE)
//	[2..3]  hotspot Y (int16 BE)
//	[4..5]  width (int16 BE)
//	[6..7]  height (int16 BE)
//	[8..]   RGBA bitmap data (width * height * 4 bytes)
func (b *bridge) handleCursorShape(f apcp.AVPTFrame) {
	if len(f.Payload) < 8 {
		return
	}
	hotX := int16(binary.BigEndian.Uint16(f.Payload[0:2]))
	hotY := int16(binary.BigEndian.Uint16(f.Payload[2:4]))
	w := int(binary.BigEndian.Uint16(f.Payload[4:6]))
	h := int(binary.BigEndian.Uint16(f.Payload[6:8]))

	expectedLen := 8 + w*h*4
	if len(f.Payload) < expectedLen {
		log.Printf("bridge: cursor frame 136 too short: got %d, need %d for %dx%d",
			len(f.Payload), expectedLen, w, h)
		return
	}

	log.Printf("bridge: cursor shape %dx%d hotspot=(%d,%d)", w, h, hotX, hotY)

	// Pack as: "CURS" magic (4b) + hotX (2b) + hotY (2b) + w (2b) + h (2b) + RGBA pixels
	buf := make([]byte, 12+w*h*4)
	copy(buf[0:4], []byte("CURS"))
	binary.BigEndian.PutUint16(buf[4:6], uint16(hotX))
	binary.BigEndian.PutUint16(buf[6:8], uint16(hotY))
	binary.BigEndian.PutUint16(buf[8:10], uint16(w))
	binary.BigEndian.PutUint16(buf[10:12], uint16(h))
	copy(buf[12:], f.Payload[8:])

	b.sinkMu.Lock()
	for _, s := range b.subs {
		select {
		case s.send <- buf:
		default:
		}
	}
	b.sinkMu.Unlock()
}

// handlePaletteUpdate parses frame 138 (VGA palette change) and applies it
// to the palette decoder (dec7p). Payload layout (Java b.a.a in
// com.avocent.kvm.a.a):
//
//	[0..7]   AVPT tile header (x, y, w, h — unused for palette frames)
//	[8]      start_index (first palette slot to update)
//	[9]      count (number of entries)
//	[10..]   count × 3 bytes RGB (or 4 bytes RGBA depending on firmware)
//
// The BMC uses this only in Depth7Palette mode; modern firmware serves
// 15-bit or 21-bit RGB and never sends this frame. We accept both 3-byte
// and 4-byte per-entry layouts.
func (b *bridge) handlePaletteUpdate(f apcp.AVPTFrame) {
	if len(f.Payload) < 10 {
		return
	}
	start := int(f.Payload[8])
	count := int(f.Payload[9])
	if start+count > 128 {
		count = 128 - start
	}
	if count <= 0 {
		return
	}
	body := f.Payload[10:]
	// Detect entry size: prefer 4 bytes if payload matches, else 3.
	entrySize := 3
	if len(body) >= count*4 {
		entrySize = 4
	} else if len(body) < count*3 {
		log.Printf("bridge: palette frame 138 truncated: bodyLen=%d count=%d", len(body), count)
		return
	}
	entries := make([]uint32, start+count)
	for i := 0; i < count; i++ {
		off := i * entrySize
		r := uint32(body[off])
		g := uint32(body[off+1])
		bl := uint32(body[off+2])
		entries[start+i] = 0xFF000000 | r<<16 | g<<8 | bl
	}
	b.dec7p.SetPalette(entries)
	log.Printf("bridge: palette update start=%d count=%d entrySize=%d", start, count, entrySize)
}

// pushLoop sends a full framebuffer PNG-ish binary payload to all
// subscribers at ~20 fps when dirty.
func (b *bridge) pushLoop() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		b.fbMu.Lock()
		if !b.fb.Dirty {
			b.fbMu.Unlock()
			continue
		}
		b.fb.Dirty = false
		buf := b.serializeFB()
		b.fbMu.Unlock()
		b.sinkMu.Lock()
		for _, s := range b.subs {
			select {
			case s.send <- buf:
			default:
			}
		}
		b.sinkMu.Unlock()
	}
}

// serializeFB packs the framebuffer as a compact binary the browser can
// blit to a canvas: 4-byte magic "IKVM", 2-byte width, 2-byte height, then
// width*height uint32 pixels in RGBA order. Callers must hold b.fbMu.
func (b *bridge) serializeFB() []byte {
	w, h := b.fb.W, b.fb.H
	buf := make([]byte, 8+4*w*h)
	copy(buf[0:4], "IKVM")
	binary.BigEndian.PutUint16(buf[4:6], uint16(w))
	binary.BigEndian.PutUint16(buf[6:8], uint16(h))
	// framebuffer is ARGB uint32; browser wants RGBA
	for i, p := range b.fb.Pixels {
		off := 8 + i*4
		buf[off+0] = byte(p >> 16) // R
		buf[off+1] = byte(p >> 8)  // G
		buf[off+2] = byte(p)       // B
		buf[off+3] = byte(p >> 24) // A
	}
	return buf
}

// handleScreenPNG renders the current framebuffer as a PNG. Diagnostic /
// screenshot endpoint — lets the operator (and tooling) confirm the live
// decode without a browser canvas.
func (b *bridge) handleScreenPNG(w http.ResponseWriter, r *http.Request) {
	b.fbMu.Lock()
	img := image.NewRGBA(image.Rect(0, 0, b.fb.W, b.fb.H))
	for i, p := range b.fb.Pixels {
		img.Pix[i*4+0] = byte(p >> 16)
		img.Pix[i*4+1] = byte(p >> 8)
		img.Pix[i*4+2] = byte(p)
		img.Pix[i*4+3] = 0xFF
	}
	b.fbMu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_ = png.Encode(w, img)
}

// handleCert opens a one-shot TLS session to the BMC and returns its leaf
// certificate details (subject/issuer/validity/fingerprint) so the operator can
// see the expiry and whether it matches the pinned fingerprint. Read-only.
func (b *bridge) handleCert(w http.ResponseWriter, r *http.Request) {
	// Serve the leaf certificate captured during the live APCP TLS handshake —
	// the BMC resets extra connections on the KVM port and its HTTPS port speaks
	// a stack we can't cleanly renegotiate, so reusing the session's cert is the
	// reliable source.
	cert := tlsdial.LastLeaf()
	if cert == nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Errorf("no BMC certificate captured yet"))
		return
	}
	now := time.Now()
	fp := tlsdial.FingerprintSHA256(cert)
	writeJSON(w, map[string]any{
		"subject":             cert.Subject.String(),
		"issuer":              cert.Issuer.String(),
		"not_before":          cert.NotBefore.UTC().Format(time.RFC3339),
		"not_after":           cert.NotAfter.UTC().Format(time.RFC3339),
		"expired":             now.After(cert.NotAfter) || now.Before(cert.NotBefore),
		"days_remaining":      int(cert.NotAfter.Sub(now).Hours() / 24),
		"serial":              cert.SerialNumber.String(),
		"signature_algorithm": cert.SignatureAlgorithm.String(),
		"fingerprint_sha256":  fp,
		"pin_matches":         fp == b.pin,
	})
}

// handleSmartcard reports the local PC/SC smart-card reader + card status so
// the UI can show whether CAC/PIV redirection is possible. Read-only probe.
func (b *bridge) handleSmartcard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, vcard.Status())
}

// serveWS upgrades to WebSocket. Requires a valid token and same-origin.
func (b *bridge) serveWS(w http.ResponseWriter, r *http.Request) {
	if !b.checkToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{
		CheckOrigin: sameOrigin,
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	sub := &subscriber{conn: c, send: make(chan []byte, 4), done: make(chan struct{})}
	b.sinkMu.Lock()
	b.subs = append(b.subs, sub)
	b.sinkMu.Unlock()
	go b.readWS(sub)
	go b.writeWS(sub)

	// Send an initial full frame immediately.
	b.fbMu.Lock()
	buf := b.serializeFB()
	b.fbMu.Unlock()
	select {
	case sub.send <- buf:
	default:
	}
}

func (b *bridge) writeWS(sub *subscriber) {
	defer func() {
		close(sub.done)
		sub.conn.Close()
	}()
	for {
		select {
		case msg := <-sub.send:
			if err := sub.conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
		}
	}
}

func (b *bridge) readWS(sub *subscriber) {
	for {
		_, msg, err := sub.conn.ReadMessage()
		if err != nil {
			return
		}
		var evt struct {
			Type    string `json:"type"`
			Key     uint16 `json:"key,omitempty"`
			Pressed bool   `json:"pressed,omitempty"`
			X       int    `json:"x,omitempty"`
			Y       int    `json:"y,omitempty"`
			Buttons byte   `json:"buttons,omitempty"`
			Wheel   int    `json:"wheel,omitempty"`
		}
		if err := json.Unmarshal(msg, &evt); err != nil {
			continue
		}
		var frame []byte
		switch evt.Type {
		case "key":
			frame = input.KeyEvent(evt.Key, evt.Pressed)
		case "mouse":
			frame = input.MouseAbsolute(evt.X, evt.Y, evt.Buttons, evt.Wheel)
		case "refresh":
			// Force the BMC to resend a full frame (re-arm video-enable +
			// re-request initial frame). Lets the client repaint even when
			// the screen is static and the BMC withholds change-tiles.
			b.writeMu.Lock()
			_, _ = b.sess.Conn.Write(apcp.AVPTFrame{ID: 782, Payload: []byte{1, 1, 0, 0, 0, 0, 0, 0}}.Encode())
			_, _ = b.sess.Conn.Write(apcp.AVPTFrame{ID: 772, Payload: make([]byte, 8)}.Encode())
			b.writeMu.Unlock()
			continue
		default:
			continue
		}
		if frame != nil {
			b.writeMu.Lock()
			_, err := b.sess.Conn.Write(frame)
			b.writeMu.Unlock()
			if err != nil {
				log.Printf("input write error: %v", err)
			}
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// handleVMMount attaches a media file (already on the server FS) via a new
// KVM-VM subchannel. Body: {"type":"cd"|"usb"|"floppy","path":"...","writable":bool}.
func (b *bridge) handleVMMount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	var media *vm.Media
	var err error
	switch req.Type {
	case "cd":
		media, err = vm.OpenCD(req.Path)
	case "usb":
		media, err = vm.OpenUSB(req.Path, false, 0)
	case "floppy":
		media, err = vm.OpenFloppy(req.Path, req.Writable)
	default:
		http.Error(w, "type must be cd|usb|floppy", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "open media: "+err.Error(), http.StatusBadRequest)
		return
	}

	m, err := b.mountMediaSubchannel(media)
	if err != nil {
		media.Close()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// tempPath intentionally left empty — /api/vm/mount attaches server-local
	// files that the operator owns and should not be auto-deleted on unmount.
	// Uploaded files (via /api/vm/upload) set tempPath themselves.

	writeJSON(w, mountStatus(m))
}

// handleVMUnmount detaches the given drive_id. Body: {"drive_id":0}.
func (b *bridge) handleVMUnmount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DriveID uint16 `json:"drive_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	b.mountsMu.Lock()
	m, ok := b.mounts[req.DriveID]
	b.mountsMu.Unlock()
	if !ok {
		http.Error(w, "no such mount", http.StatusNotFound)
		return
	}
	// Detach announces VDISK_RELEASE; then closing the subchannel conn
	// unblocks the Run() loop, which handles cleanup + map removal.
	if err := m.sess.Detach(m.driveID); err != nil {
		log.Printf("bridge: detach: %v", err)
	}
	m.conn.Close()
	writeJSON(w, map[string]any{"ok": true, "drive_id": req.DriveID})
}

// handleVMStatus returns the list of active mounts as JSON.
func (b *bridge) handleVMStatus(w http.ResponseWriter, r *http.Request) {
	b.mountsMu.Lock()
	out := make([]map[string]any, 0, len(b.mounts))
	for _, m := range b.mounts {
		out = append(out, mountStatus(m))
	}
	b.mountsMu.Unlock()
	writeJSON(w, out)
}

// handleVMUpload accepts a multipart file, stores it in os.TempDir, and
// immediately mounts it. Client should later call /api/vm/unmount to
// release; the temp file is deleted when the mount tears down.
// Query params: type=cd|usb|floppy (default cd), writable=1 (floppy only).
func (b *bridge) handleVMUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	mediaType := r.URL.Query().Get("type")
	if mediaType == "" {
		mediaType = "cd"
	}
	writable := r.URL.Query().Get("writable") == "1"

	// 8 GiB cap; multipart streams to disk beyond 32 MiB automatically.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "form file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	dir, err := os.MkdirTemp("", "ikvm-vm-")
	if err != nil {
		http.Error(w, "temp dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dstPath := filepath.Join(dir, filepath.Base(hdr.Filename))
	dst, err := os.Create(dstPath)
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		os.RemoveAll(dir)
		http.Error(w, "copy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dst.Close()

	var media *vm.Media
	switch mediaType {
	case "cd":
		media, err = vm.OpenCD(dstPath)
	case "usb":
		media, err = vm.OpenUSB(dstPath, false, 0)
	case "floppy":
		media, err = vm.OpenFloppy(dstPath, writable)
	default:
		os.RemoveAll(dir)
		http.Error(w, "type must be cd|usb|floppy", http.StatusBadRequest)
		return
	}
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "open media: "+err.Error(), http.StatusBadRequest)
		return
	}

	m, err := b.mountMediaSubchannel(media)
	if err != nil {
		media.Close()
		os.RemoveAll(dir)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// Track the whole temp dir so cleanup removes the parent, not just the file.
	m.tempPath = dir

	writeJSON(w, mountStatus(m))
}

// mountStatus renders a mount as a plain map for JSON encoding.
func mountStatus(m *mount) map[string]any {
	return map[string]any{
		"drive_id":      m.driveID,
		"type":          m.media.Type(),
		"path":          m.media.Path,
		"blocks":        m.media.Blocks(),
		"block_size":    m.media.BlockSize(),
		"bytes_read":    m.media.BytesRead(),
		"bytes_written": m.media.BytesWritten(),
		"mounted_at":    m.mountedAt.UTC().Format(time.RFC3339),
	}
}

// unused imports guard - keep referenced
var _ = context.Background
var _ = rand.Read
var _ = big.NewInt
