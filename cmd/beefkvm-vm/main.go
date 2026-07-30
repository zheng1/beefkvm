// Command ikvm-vm attaches a local disk image to the BMC as virtual media.
// Supports CD-ROM (ISO), floppy, and writable USB disk images.
//
// Ported from ./start-virtual-media.sh + com.avocent.vm.VirtualMedia:
// opens an APCP session of SessionTypeVM (=2), authenticates, then runs
// the AVMP protocol loop from internal/vm.
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
		iso    = flag.String("iso", "", "path to ISO file to mount as CD-ROM")
		floppy = flag.String("floppy", "", "path to floppy image (1.44MB/720KB/1.2MB)")
		usb    = flag.String("usb", "", "path to USB disk image (writable)")
	)
	flag.Parse()

	if *iso == "" && *floppy == "" && *usb == "" {
		log.Fatal("one of --iso, --floppy, or --usb required")
	}
	if (*iso != "" && *floppy != "") || (*iso != "" && *usb != "") || (*floppy != "" && *usb != "") {
		log.Fatal("only one of --iso, --floppy, or --usb allowed")
	}
	if *pass == "" || *pin == "" {
		log.Fatal("BMC_PASS and BMC_PIN (or --pass/--pin) required")
	}

	// Open media file.
	var media *vm.Media
	var err error
	switch {
	case *iso != "":
		media, err = vm.OpenCD(*iso)
		if err != nil {
			log.Fatalf("open ISO %s: %v", *iso, err)
		}
		log.Printf("opened %s: %d blocks × 2048 bytes (CD-ROM)", *iso, media.Blocks())
	case *floppy != "":
		media, err = vm.OpenFloppy(*floppy, false)
		if err != nil {
			log.Fatalf("open floppy %s: %v", *floppy, err)
		}
		log.Printf("opened %s: %d blocks × 512 bytes (floppy)", *floppy, media.Blocks())
	case *usb != "":
		media, err = vm.OpenUSB(*usb, false, 0)
		if err != nil {
			log.Fatalf("open USB %s: %v", *usb, err)
		}
		log.Printf("opened %s: %d blocks × 512 bytes (USB, writable)", *usb, media.Blocks())
	}
	defer media.Close()

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
