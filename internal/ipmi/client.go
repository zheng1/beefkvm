package ipmi

import (
	"fmt"
	"strings"
	"sync"
)

// Client is the high-level entry point used by both the CLI and the web
// bridge. It owns a Session and serializes access. Safe for concurrent
// use — internal RMCP+ session sequence numbers require ordering.
type Client struct {
	mu   sync.Mutex
	sess *Session
	host string
	user string
	pass string
}

// Open dials the BMC and completes the RAKP handshake. Returns an
// authenticated Client, or an error whose text includes the RAKP2/4
// status code when the handshake was rejected.
func Open(host, user, pass string) (*Client, error) {
	s, err := Dial(host, user, pass)
	if err != nil {
		return nil, err
	}
	return &Client{sess: s, host: host, user: user, pass: pass}, nil
}

// Close terminates the IPMI session (sends Close Session best-effort)
// and closes the UDP socket.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return nil
	}
	err := c.sess.Close()
	c.sess = nil
	return err
}

// PowerStatus returns the current chassis power state and last-event
// summary. Callers can pattern-match on Status.PowerOn.
func (c *Client) PowerStatus() (*ChassisStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.getChassisStatus()
}

// PowerOn asserts chassis power. No-op (returns cc=0x00) if already on.
func (c *Client) PowerOn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.chassisControl(ChassisUp)
}

// PowerOff cuts chassis power immediately (hard off).
func (c *Client) PowerOff() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.chassisControl(ChassisDown)
}

// PowerReset issues a hard reset (equivalent to front-panel reset button).
func (c *Client) PowerReset() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.chassisControl(ChassisReset)
}

// PowerSoftShutdown asks the OS to shut down cleanly via ACPI.
func (c *Client) PowerSoftShutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.chassisControl(ChassisSoft)
}

// PowerCycle powers the chassis down and back up.
func (c *Client) PowerCycle() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.chassisControl(ChassisCycle)
}

// SetOneTimeBoot programs a next-boot override. The BMC clears the
// override after one boot.
func (c *Client) SetOneTimeBoot(dev string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b BootDevice
	switch strings.ToLower(dev) {
	case "none":
		b = BootNoOverride
	case "pxe":
		b = BootPXE
	case "hdd", "disk":
		b = BootHDD
	case "cd", "cdrom", "dvd":
		b = BootCD
	case "bios", "setup":
		b = BootBIOS
	default:
		return fmt.Errorf("ipmi: unknown boot device %q", dev)
	}
	return c.sess.setBootDevice(b)
}

// Send issues an arbitrary IPMI request over the authenticated session.
// The returned slice is the response body with the completion code
// prepended as byte[0] (matching the convention used by the SDR/SEL/
// sensor readers): callers should check resp[0] == 0 before parsing.
// Serialised through c.mu so it's safe to call from multiple goroutines
// (e.g. the /api/sensors and /api/power handlers concurrently).
func (c *Client) Send(netFn, cmd byte, data []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return nil, fmt.Errorf("ipmi: session closed")
	}
	cc, out, err := c.sess.rawCall(netFn, cmd, data)
	if err != nil {
		return nil, err
	}
	// Prepend completion code so all downstream parsers use a single
	// shape (resp[0]=cc, resp[1:]=body).
	resp := make([]byte, 1+len(out))
	resp[0] = cc
	copy(resp[1:], out)
	return resp, nil
}
