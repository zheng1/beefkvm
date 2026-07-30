// Command ikvm-power is a thin CLI wrapper for the internal/ipmi package
// so operators (and this project's tests) can drive chassis power on the
// same iDRAC/Avocent BMC used by ikvm-web without going through the
// browser. Same host/user/pass conventions as the sibling commands.
//
// Usage:
//
//	ikvm-power --host X --user Y --pass Z status
//	ikvm-power --host X --user Y --pass Z on|off|cycle|reset|soft
//	ikvm-power --host X --user Y --pass Z boot=pxe|cd|hdd|bios
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/zheng1/beefkvm/internal/ipmi"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		host = flag.String("host", envOr("BMC_HOST", ""), "BMC host")
		user = flag.String("user", envOr("BMC_USER", "admin"), "BMC user")
		pass = flag.String("pass", envOr("BMC_PASS", ""), "BMC password")
	)
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ikvm-power [flags] <status|on|off|cycle|reset|soft|boot=pxe|boot=cd|boot=hdd|boot=bios>")
		os.Exit(2)
	}
	if *pass == "" {
		log.Fatal("ikvm-power: BMC_PASS (or --pass) required")
	}

	c, err := ipmi.Open(*host, *user, *pass)
	if err != nil {
		log.Fatalf("ikvm-power: open: %v", err)
	}
	defer c.Close()

	cmd := args[0]
	switch {
	case cmd == "status":
		st, err := c.PowerStatus()
		if err != nil {
			log.Fatalf("ikvm-power: status: %v", err)
		}
		state := "off"
		if st.PowerOn {
			state = "on"
		}
		fmt.Printf("power=%s last-event=%s raw=%x\n", state, st.LastEvent, st.Raw)
	case cmd == "on":
		must(c.PowerOn())
		fmt.Println("power on: ok")
	case cmd == "off":
		must(c.PowerOff())
		fmt.Println("power off: ok")
	case cmd == "cycle":
		must(c.PowerCycle())
		fmt.Println("power cycle: ok")
	case cmd == "reset":
		must(c.PowerReset())
		fmt.Println("power reset: ok")
	case cmd == "soft":
		must(c.PowerSoftShutdown())
		fmt.Println("soft shutdown: ok")
	case strings.HasPrefix(cmd, "boot="):
		dev := strings.TrimPrefix(cmd, "boot=")
		must(c.SetOneTimeBoot(dev))
		fmt.Printf("one-time boot -> %s: ok\n", dev)
	default:
		log.Fatalf("ikvm-power: unknown command %q", cmd)
	}
}

func must(err error) {
	if err != nil {
		log.Fatalf("ikvm-power: %v", err)
	}
}
