package ipmi

import (
	"os"
	"sync"
	"testing"
	"time"
)

// TestSOLLive activates SOL against a real BMC when IKVM_SOL_LIVE=1. It proves
// the native Activate Payload + packet path works end to end. Run:
//
//	IKVM_SOL_LIVE=1 SOL_HOST=bmc.example SOL_USER=admin SOL_PASS=secret \
//	  go test ./internal/ipmi -run TestSOLLive -v
func TestSOLLive(t *testing.T) {
	if os.Getenv("IKVM_SOL_LIVE") != "1" {
		t.Skip("set IKVM_SOL_LIVE=1 to run the live SOL test")
	}
	host := os.Getenv("SOL_HOST")
	user := os.Getenv("SOL_USER")
	pass := os.Getenv("SOL_PASS")

	var mu sync.Mutex
	var got []byte
	sol, err := OpenSOL(host, user, pass, func(b []byte) {
		mu.Lock()
		got = append(got, b...)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("OpenSOL: %v", err)
	}
	t.Log("SOL activated OK")

	// Poke the serial line and watch for any echo/output.
	if err := sol.Write([]byte("\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(3 * time.Second)
	if err := sol.Write([]byte("\r\n")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	time.Sleep(2 * time.Second)

	mu.Lock()
	n := len(got)
	preview := string(got)
	mu.Unlock()
	t.Logf("received %d serial bytes: %q", n, preview)
	t.Logf("BMC ACKs for our TX: %d", sol.AcksSeen())
	if sol.AcksSeen() == 0 {
		t.Errorf("no SOL ACKs from BMC — console→BMC TX path not confirmed")
	}

	if err := sol.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	t.Log("SOL deactivated OK")
}
