package ipmi

import "fmt"

// LANConfig holds the commonly-read LAN parameters for one channel (IPMI 2.0
// §23.1 Get LAN Configuration Parameters, NetFn Transport 0x0C cmd 0x02).
type LANConfig struct {
	Channel    byte   `json:"channel"`
	IPSource   string `json:"ip_source"` // static / dhcp / bios / other
	IP         string `json:"ip"`
	SubnetMask string `json:"subnet_mask"`
	Gateway    string `json:"gateway"`
	MAC        string `json:"mac"`
	GatewayMAC string `json:"gateway_mac,omitempty"`
	VLANID     int    `json:"vlan_id"` // -1 if disabled
}

// lanChannel is the primary LAN channel on this BMC.
const lanChannel = 0x01

func (c *Client) getLANParam(param, set byte) ([]byte, error) {
	resp, err := c.Send(0x0c, 0x02, []byte{lanChannel, param, set, 0x00})
	if err != nil {
		return nil, err
	}
	if resp[0] != 0 {
		return nil, fmt.Errorf("ipmi: Get LAN Param %d cc=0x%02x", param, resp[0])
	}
	// resp[1] = param revision; data starts at resp[2].
	if len(resp) < 2 {
		return nil, fmt.Errorf("ipmi: short LAN param %d", param)
	}
	return resp[2:], nil
}

func ipv4(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}
func macStr(b []byte) string {
	if len(b) < 6 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// GetLANConfig reads the key LAN parameters. Individual params that error are
// left at their zero value so a partial read still returns useful data.
func (c *Client) GetLANConfig() (*LANConfig, error) {
	cfg := &LANConfig{Channel: lanChannel, VLANID: -1}

	if d, err := c.getLANParam(4, 0); err == nil && len(d) >= 1 {
		switch d[0] & 0x0f {
		case 1:
			cfg.IPSource = "static"
		case 2:
			cfg.IPSource = "dhcp"
		case 3:
			cfg.IPSource = "bios"
		case 4:
			cfg.IPSource = "other"
		default:
			cfg.IPSource = fmt.Sprintf("0x%x", d[0]&0x0f)
		}
	}
	if d, err := c.getLANParam(3, 0); err == nil {
		cfg.IP = ipv4(d)
	}
	if d, err := c.getLANParam(6, 0); err == nil {
		cfg.SubnetMask = ipv4(d)
	}
	if d, err := c.getLANParam(12, 0); err == nil {
		cfg.Gateway = ipv4(d)
	}
	if d, err := c.getLANParam(5, 0); err == nil {
		cfg.MAC = macStr(d)
	}
	if d, err := c.getLANParam(13, 0); err == nil {
		cfg.GatewayMAC = macStr(d)
	}
	if d, err := c.getLANParam(20, 0); err == nil && len(d) >= 2 {
		if d[1]&0x80 != 0 { // VLAN enabled
			cfg.VLANID = int(d[0]) | int(d[1]&0x0f)<<8
		}
	}
	return cfg, nil
}

// SetLANParam writes one raw LAN configuration parameter (Set LAN Config
// Parameters, 0x0C/0x01). Caller supplies the parameter-specific data bytes.
// Changing IP/mask/gateway on the channel you're connected through can drop
// BMC connectivity — callers must gate this behind explicit confirmation.
func (c *Client) SetLANParam(param byte, data []byte) error {
	req := make([]byte, 0, 2+len(data))
	req = append(req, lanChannel, param)
	req = append(req, data...)
	return c.sendCheck(fmt.Sprintf("Set LAN Param %d", param), 0x0c, 0x01, req)
}

// SetIPSource sets LAN parameter 4 (1=static, 2=dhcp). Convenience wrapper.
func (c *Client) SetIPSource(src byte) error {
	return c.SetLANParam(4, []byte{src & 0x0f})
}
