package ipmi

import "fmt"

// DeviceID is the parsed response of Get Device ID (NetFn App 0x06, cmd 0x01).
type DeviceID struct {
	DeviceID       byte   `json:"device_id"`
	DeviceRevision byte   `json:"device_revision"`
	FirmwareMajor  byte   `json:"firmware_major"`
	FirmwareMinor  byte   `json:"firmware_minor"` // BCD-encoded
	FirmwareRev    string `json:"firmware_rev"`   // "major.minor" display form
	IPMIVersion    string `json:"ipmi_version"`
	ManufacturerID uint32 `json:"manufacturer_id"`
	ProductID      uint16 `json:"product_id"`
}

// GetDeviceID queries basic BMC identity + firmware version. §20.1.
func (c *Client) GetDeviceID() (*DeviceID, error) {
	resp, err := c.Send(0x06, 0x01, nil)
	if err != nil {
		return nil, err
	}
	if resp[0] != 0 {
		return nil, fmt.Errorf("ipmi: Get Device ID cc=0x%02x", resp[0])
	}
	b := resp[1:]
	if len(b) < 11 {
		return nil, fmt.Errorf("ipmi: short Get Device ID response (%d bytes)", len(b))
	}
	d := &DeviceID{
		DeviceID:       b[0],
		DeviceRevision: b[1] & 0x0f,
		FirmwareMajor:  b[2] & 0x7f,
		FirmwareMinor:  b[3],
		IPMIVersion:    fmt.Sprintf("%d.%d", b[4]&0x0f, b[4]>>4),
		ManufacturerID: uint32(b[6]) | uint32(b[7])<<8 | uint32(b[8])<<16,
		ProductID:      uint16(b[9]) | uint16(b[10])<<8,
	}
	// Firmware minor is BCD (e.g. 0x34 → "34").
	d.FirmwareRev = fmt.Sprintf("%d.%02x", d.FirmwareMajor, d.FirmwareMinor)
	return d, nil
}

// FRU holds the human-readable fields of a FRU device's Board and Product
// info areas (IPMI Platform Management FRU Information Storage spec).
type FRU struct {
	BoardMfg       string `json:"board_mfg,omitempty"`
	BoardProduct   string `json:"board_product,omitempty"`
	BoardSerial    string `json:"board_serial,omitempty"`
	BoardPart      string `json:"board_part,omitempty"`
	ProductMfg     string `json:"product_mfg,omitempty"`
	ProductName    string `json:"product_name,omitempty"`
	ProductPart    string `json:"product_part,omitempty"`
	ProductVersion string `json:"product_version,omitempty"`
	ProductSerial  string `json:"product_serial,omitempty"`
	ProductAsset   string `json:"product_asset,omitempty"`
}

// ReadFRU reads and parses FRU device fruID (0 = primary/motherboard).
func (c *Client) ReadFRU(fruID byte) (*FRU, error) {
	// Get FRU Inventory Area Info (NetFn Storage 0x0A, cmd 0x10).
	info, err := c.Send(0x0a, 0x10, []byte{fruID})
	if err != nil {
		return nil, err
	}
	if info[0] != 0 {
		return nil, fmt.Errorf("ipmi: Get FRU Area Info cc=0x%02x", info[0])
	}
	if len(info) < 3 {
		return nil, fmt.Errorf("ipmi: short FRU area info")
	}
	size := int(info[1]) | int(info[2])<<8
	if size <= 0 || size > 8192 {
		size = 256 // sane cap for a malformed/huge report
	}

	// Read the whole FRU in small chunks (some BMCs cap the read count).
	data := make([]byte, 0, size)
	off := 0
	for off < size {
		n := 16
		if off+n > size {
			n = size - off
		}
		r, err := c.Send(0x0a, 0x11, []byte{fruID, byte(off), byte(off >> 8), byte(n)})
		if err != nil {
			return nil, err
		}
		if r[0] != 0 || len(r) < 2 {
			break // stop on the first read error; parse what we have
		}
		got := int(r[1])
		if got == 0 || len(r) < 2+got {
			break
		}
		data = append(data, r[2:2+got]...)
		off += got
	}
	return parseFRU(data)
}

// parseFRU walks the FRU common header to the Board and Product info areas and
// extracts their type/length-encoded string fields in spec order.
func parseFRU(d []byte) (*FRU, error) {
	if len(d) < 8 || d[0] != 0x01 {
		return nil, fmt.Errorf("ipmi: not a v1 FRU image (%d bytes)", len(d))
	}
	f := &FRU{}
	boardOff := int(d[3]) * 8
	productOff := int(d[4]) * 8

	if boardOff > 0 && boardOff+6 < len(d) {
		// Board area: version, length, lang, 3-byte mfg date, then strings.
		p := boardOff + 6
		f.BoardMfg, p = fruString(d, p)
		f.BoardProduct, p = fruString(d, p)
		f.BoardSerial, p = fruString(d, p)
		f.BoardPart, p = fruString(d, p)
	}
	if productOff > 0 && productOff+3 < len(d) {
		// Product area: version, length, lang, then strings.
		p := productOff + 3
		f.ProductMfg, p = fruString(d, p)
		f.ProductName, p = fruString(d, p)
		f.ProductPart, p = fruString(d, p)
		f.ProductVersion, p = fruString(d, p)
		f.ProductSerial, p = fruString(d, p)
		f.ProductAsset, p = fruString(d, p)
	}
	return f, nil
}

// fruString reads one type/length-encoded field at offset p and returns the
// decoded ASCII string plus the offset of the next field. A 0xC1 byte marks
// end-of-fields; an out-of-range offset returns "" and stops.
func fruString(d []byte, p int) (string, int) {
	if p < 0 || p >= len(d) {
		return "", p
	}
	tl := d[p]
	if tl == 0xC1 { // end-of-area marker
		return "", len(d)
	}
	length := int(tl & 0x3f)
	start := p + 1
	end := start + length
	if end > len(d) {
		return "", len(d)
	}
	// Type 3 (bits 7:6 == 11) is 8-bit ASCII/Latin1; other encodings (BCD,
	// packed) are rare in board/product areas — fall back to raw bytes.
	return string(d[start:end]), end
}
