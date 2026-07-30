package ipmi

import (
	"os"
	"testing"
)

func TestLANLive(t *testing.T) {
	if os.Getenv("IKVM_LAN_LIVE") != "1" {
		t.Skip("set IKVM_LAN_LIVE=1")
	}
	c, err := Open(os.Getenv("SOL_HOST"), os.Getenv("SOL_USER"), os.Getenv("SOL_PASS"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cfg, err := c.GetLANConfig()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("src=%s ip=%s mask=%s gw=%s mac=%s vlan=%d", cfg.IPSource, cfg.IP, cfg.SubnetMask, cfg.Gateway, cfg.MAC, cfg.VLANID)
	// GUARDED no-op write: set IP source to its CURRENT value (proves write path, changes nothing).
	cur := byte(1)
	if cfg.IPSource == "dhcp" {
		cur = 2
	}
	if err := c.SetIPSource(cur); err != nil {
		t.Errorf("no-op SetIPSource: %v", err)
	} else {
		t.Logf("no-op write OK (ip source stayed %s)", cfg.IPSource)
	}
}
