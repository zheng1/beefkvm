package ipmi

import (
	"bytes"
	"fmt"
)

// User is one BMC user slot as reported by Get User Access + Get User Name.
type User struct {
	ID        byte   `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Privilege byte   `json:"privilege"` // 1=Callback 2=User 3=Operator 4=Admin 0xF=NoAccess
	PrivName  string `json:"privilege_name"`
	MsgEnable bool   `json:"msg_enabled"` // IPMI messaging enabled on the channel
	LinkAuth  bool   `json:"link_auth"`
}

// privName maps an IPMI privilege-limit nibble to a label.
func privName(p byte) string {
	switch p & 0x0f {
	case 1:
		return "Callback"
	case 2:
		return "User"
	case 3:
		return "Operator"
	case 4:
		return "Administrator"
	case 5:
		return "OEM"
	case 0x0f:
		return "No Access"
	default:
		return fmt.Sprintf("0x%x", p&0x0f)
	}
}

// userChannel is the LAN channel used for user access queries (this BMC's
// primary LAN + SOL live on channel 1).
const userChannel = 0x01

// ListUsers walks every user slot on the LAN channel, returning name,
// privilege, and enable state. Empty/unnamed slots are included so the UI can
// offer them for provisioning.
func (c *Client) ListUsers() ([]User, error) {
	// First query slot 1 to learn the max user count.
	first, err := c.getUserAccess(1)
	if err != nil {
		return nil, err
	}
	max := first.maxIDs
	if max == 0 || max > 63 {
		max = 15
	}
	users := make([]User, 0, max)
	for id := byte(1); id <= max; id++ {
		ua := first
		if id != 1 {
			ua, err = c.getUserAccess(id)
			if err != nil {
				continue // skip a slot that errors, keep going
			}
		}
		name, _ := c.GetUserName(id)
		users = append(users, User{
			ID:        id,
			Name:      name,
			Enabled:   ua.enabled,
			Privilege: ua.priv,
			PrivName:  privName(ua.priv),
			MsgEnable: ua.msgEnable,
			LinkAuth:  ua.linkAuth,
		})
	}
	return users, nil
}

type userAccess struct {
	maxIDs    byte
	enabled   bool
	priv      byte
	msgEnable bool
	linkAuth  bool
}

// getUserAccess issues Get User Access (NetFn App 0x06, cmd 0x44) for one slot.
func (c *Client) getUserAccess(id byte) (userAccess, error) {
	resp, err := c.Send(0x06, 0x44, []byte{userChannel & 0x0f, id & 0x3f})
	if err != nil {
		return userAccess{}, err
	}
	if resp[0] != 0 {
		return userAccess{}, fmt.Errorf("ipmi: Get User Access cc=0x%02x", resp[0])
	}
	b := resp[1:]
	if len(b) < 4 {
		return userAccess{}, fmt.Errorf("ipmi: short Get User Access response")
	}
	// b[0][5:0]=max IDs; b[1][7:6]=enable status (01=enabled,10=disabled);
	// b[3][3:0]=priv limit, [4]=msg enable, [5]=link auth.
	return userAccess{
		maxIDs:    b[0] & 0x3f,
		enabled:   (b[1]>>6)&0x03 == 0x01,
		priv:      b[3] & 0x0f,
		msgEnable: b[3]&0x10 != 0,
		linkAuth:  b[3]&0x20 != 0,
	}, nil
}

// GetUserName issues Get User Name (0x46); returns the trimmed ASCII name.
func (c *Client) GetUserName(id byte) (string, error) {
	resp, err := c.Send(0x06, 0x46, []byte{id & 0x3f})
	if err != nil {
		return "", err
	}
	if resp[0] != 0 {
		return "", fmt.Errorf("ipmi: Get User Name cc=0x%02x", resp[0])
	}
	name := resp[1:]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return string(bytes.TrimRight(name, " ")), nil
}

// SetUserName issues Set User Name (0x45). Name is truncated/zero-padded to 16.
func (c *Client) SetUserName(id byte, name string) error {
	data := make([]byte, 17)
	data[0] = id & 0x3f
	copy(data[1:], name)
	return c.sendCheck("Set User Name", 0x06, 0x45, data)
}

// SetUserPassword sets (op=2) the 16-byte password for a user slot (0x47).
func (c *Client) SetUserPassword(id byte, pass string) error {
	if len(pass) > 16 {
		return fmt.Errorf("ipmi: password too long (max 16 for 16-byte mode)")
	}
	data := make([]byte, 18)
	data[0] = id & 0x3f // bit7=0 → 16-byte password
	data[1] = 0x02      // operation: set password
	copy(data[2:], pass)
	return c.sendCheck("Set User Password", 0x06, 0x47, data)
}

// EnableUser / DisableUser toggle a slot via Set User Password op 1/0 (0x47).
func (c *Client) EnableUser(id byte) error {
	return c.sendCheck("Enable User", 0x06, 0x47, []byte{id & 0x3f, 0x01})
}
func (c *Client) DisableUser(id byte) error {
	return c.sendCheck("Disable User", 0x06, 0x47, []byte{id & 0x3f, 0x00})
}

// SetUserPrivilege issues Set User Access (0x43) to set the channel privilege
// limit and enable IPMI messaging + link auth for a slot.
func (c *Client) SetUserPrivilege(id, priv byte) error {
	// byte1: [7]=1 apply access bits, [6]=1 enable IPMI messaging, [5]=1 link
	// auth, [4]=0 callin, [3:0]=channel.
	b1 := byte(0x90) | (userChannel & 0x0f) // 0x80 change bits + 0x10 msg enable
	data := []byte{b1, id & 0x3f, priv & 0x0f, 0x00}
	return c.sendCheck("Set User Access", 0x06, 0x43, data)
}

// sendCheck issues a command and returns a single error, decoding the cc.
func (c *Client) sendCheck(what string, netFn, cmd byte, data []byte) error {
	resp, err := c.Send(netFn, cmd, data)
	if err != nil {
		return err
	}
	if len(resp) == 0 {
		return fmt.Errorf("ipmi: %s: empty response", what)
	}
	if resp[0] != 0 {
		return fmt.Errorf("ipmi: %s cc=0x%02x", what, resp[0])
	}
	return nil
}
