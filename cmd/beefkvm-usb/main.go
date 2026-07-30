// Command ikvm-usb attaches a local disk image to the BMC as a writable
// virtual USB drive.
//
// Similar to ikvm-vm but supports write operations.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/zheng1/beefkvm/internal/apcp"
	"github.com/zheng1/beefkvm/internal/tlsdial"
	"github.com/zheng1/beefkvm/internal/vm"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		host   = flag.String("host", envOr("BMC_HOST", ""), "BMC host")
		port   = flag.Int("port", 2068, "BMC port")
		user   = flag.String("user", envOr("BMC_USER", "admin"), "BMC user")
		pass   = flag.String("pass", envOr("BMC_PASS", ""), "BMC password")
		pin    = flag.String("pin", envOr("BMC_PIN", ""), "BMC TLS cert SHA-256 fingerprint")
		img    = flag.String("img", "", "path to disk image (required)")
		create = flag.Bool("create", false, "create image if it doesn't exist")
		size   = flag.Int64("size", 1024*1024*1024, "size in bytes for new image (default 1GB)")
	)
	flag.Parse()

	if *img == "" {
		log.Fatal("--img <path> required")
	}
	if *pass == "" || *pin == "" {
		log.Fatal("BMC_PASS and BMC_PIN (or --pass/--pin) required")
	}

	// Open USB image.
	media, err := vm.OpenUSB(*img, *create, *size)
	if err != nil {
		log.Fatalf("open USB image %s: %v", *img, err)
	}
	defer media.Close()
	log.Printf("opened %s: %d blocks × 512 bytes (writable)", *img, media.Blocks())

	// Establish VM session.
	addr := *host + ":" + itoa(*port)
	sess := &apcp.Session{
		Dial: func() (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 10*time.Second)
		},
		UpgradeTLS: func(raw net.Conn) (net.Conn, error) {
			return tlsdial.UpgradeToTLS(raw, *host, *pin)
		},
		Type:     apcp.SessionTypeVM,
		Username: *user,
		Password: *pass,
	}
	if err := sess.Open(); err != nil {
		log.Fatalf("VM session open: %v", err)
	}
	defer sess.Conn.Close()
	log.Printf("VM session ready to %s (SessionType=2)", addr)

	// Start heartbeat.
	var writeMu sync.Mutex
	stopHB := make(chan struct{})
	apcp.StartHeartbeat(sess, &writeMu, stopHB)
	defer close(stopHB)

	// Run VM protocol loop.
	vmSess := &vm.Session{
		Conn:    sess.Conn,
		WriteMu: &writeMu,
		Media:   media,
	}
	vmSess.SetLogger(log.New(os.Stderr, "[vm] ", log.LstdFlags))
	log.Printf("starting VM protocol loop; press Ctrl-C to unmount")
	if err := vmSess.Run(); err != nil {
		log.Printf("VM session ended: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
