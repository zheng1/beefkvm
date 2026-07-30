// Command apcp-capture opens a KVM session, requests video, and dumps
// each incoming video-tile packet payload to a file for offline replay.
// Used for developing the DCT decoder without hitting the BMC on every
// change.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/zheng1/beefkvm/internal/apcp"
	"github.com/zheng1/beefkvm/internal/tlsdial"
)

func main() {
	var (
		host     = flag.String("host", envOr("BMC_HOST", ""), "BMC host")
		user     = flag.String("user", envOr("BMC_USER", "admin"), "BMC user")
		pass     = flag.String("pass", envOr("BMC_PASS", ""), "BMC password")
		pin      = flag.String("pin", envOr("BMC_PIN", ""), "cert pin")
		outDir   = flag.String("out", "testdata/tiles", "output dir")
		nTiles   = flag.Int("n", 50, "number of tile packets to capture")
		videoRes = flag.String("res", "1024x768", "requested resolution")
	)
	flag.Parse()
	if *pass == "" || *pin == "" {
		log.Fatal("--pass and --pin required")
	}
	os.MkdirAll(*outDir, 0o755)

	addr := fmt.Sprintf("%s:2068", *host)
	// Primary session
	primary := &apcp.Session{
		Dial:       func() (net.Conn, error) { return net.DialTimeout("tcp", addr, 10*time.Second) },
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) { return tlsdial.UpgradeToTLS(raw, *host, *pin) },
		Type:       apcp.SessionTypeKVM,
		Username:   *user,
		Password:   *pass,
		Preempt:    true,
	}
	if err := primary.Open(); err != nil {
		log.Fatal(err)
	}
	defer primary.Conn.Close()
	log.Printf("primary session open. companion=%v", primary.KVMLoginResponse.NeedsCompanion)

	var writeMu sync.Mutex
	stop := make(chan struct{})
	apcp.StartHeartbeat(primary, &writeMu, stop)
	defer close(stop)

	// Companion socket (video)
	video := &apcp.Session{
		Dial:       func() (net.Conn, error) { return net.DialTimeout("tcp", addr, 10*time.Second) },
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) { return tlsdial.UpgradeToTLS(raw, *host, *pin) },
		Type:       apcp.SessionTypeKVMVMSubchannel,
		SkipLogin:  true,
	}
	if err := video.Open(); err != nil {
		log.Fatal(err)
	}
	defer video.Conn.Close()
	sync := apcp.EncodeVideoSyncDC(primary.KVMSessionID, primary.KVMLoginResponse.SessionID)
	video.Conn.Write(sync)
	log.Printf("companion video socket open + dc sync sent")

	// Request video stream
	var w, h int
	fmt.Sscanf(*videoRes, "%dx%d", &w, &h)
	writeMu.Lock()
	primary.Conn.Write(apcp.AVPTFrame{ID: 782, Payload: []byte{1, 1, 0, 0, 0, 0, 0, 0}}.Encode())
	resB := make([]byte, 8)
	binary.BigEndian.PutUint16(resB[0:2], uint16(w))
	binary.BigEndian.PutUint16(resB[2:4], uint16(h))
	primary.Conn.Write(apcp.AVPTFrame{ID: 770, Payload: resB}.Encode())
	primary.Conn.Write(apcp.AVPTFrame{ID: 772, Payload: make([]byte, 8)}.Encode())
	writeMu.Unlock()
	log.Printf("video enable + set-res %dx%d + initial-frame sent", w, h)

	// Capture N tile packets
	saved := 0
	for saved < *nTiles {
		f, err := apcp.ReadAVPTFrame(video.Conn)
		if err != nil {
			if err == io.EOF {
				log.Printf("video socket closed after %d tiles", saved)
				return
			}
			log.Printf("read: %v", err)
			return
		}
		if !(f.ID >= 129 && f.ID <= 138 || f.ID == 34305 || f.ID == 34306 || f.ID == 34307 || f.ID == 34310 || f.ID == 34314) {
			continue
		}
		name := fmt.Sprintf("%s/tile-%04d-id%d-len%d.bin", *outDir, saved, f.ID, len(f.Payload))
		if err := os.WriteFile(name, f.Payload, 0o644); err != nil {
			log.Printf("write: %v", err)
			return
		}
		saved++
	}
	log.Printf("captured %d tiles to %s", saved, *outDir)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
