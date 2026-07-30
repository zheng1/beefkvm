package ipmi

import (
	"encoding/binary"
	"fmt"
)

// IPMI NetFn / command constants used by the sensor + SEL surface.
const (
	NetFnSensor  byte = 0x04
	NetFnStorage byte = 0x0A

	cmdGetSDRRepoInfo  byte = 0x20
	cmdReserveSDRRepo  byte = 0x22
	cmdGetSDR          byte = 0x23
	cmdGetSensorRead   byte = 0x2D
	cmdGetSensorThresh byte = 0x27

	cmdGetSELInfo  byte = 0x40
	cmdReserveSEL  byte = 0x42
	cmdGetSELEntry byte = 0x43
	cmdClearSEL    byte = 0x47
)

// SDRType is the record-type byte in an SDR header (offset 3).
type SDRType byte

const (
	SDRTypeFull    SDRType = 0x01 // full sensor record
	SDRTypeCompact SDRType = 0x02 // compact sensor record
)

// SDRRepoInfo is the decoded response of Get SDR Repository Info.
type SDRRepoInfo struct {
	Version     byte
	RecordCount uint16
	FreeSpace   uint16
	Reservation uint16 // last reservation ID observed; zero if not reserved yet
}

// SDRHeader is the 5-byte SDR header common to all record types.
type SDRHeader struct {
	RecordID uint16
	Version  byte
	Type     SDRType
	Length   byte // body length after header
}

// Sensor is the subset of an SDR record we need to read + linearize a
// sensor value. Reserved bits and locators we don't consume are dropped.
type Sensor struct {
	RecordID   uint16
	Type       SDRType
	OwnerID    byte
	OwnerLUN   byte
	Number     byte // sensor number, passed to Get Sensor Reading
	EntityID   byte
	EntityInst byte
	SensorType byte // IPMI Table 42-3 (Temperature=0x01, Voltage=0x02, ...)
	EventType  byte // 0x01 = threshold-based
	Name       string

	// The following fields are set only for Full records (Type=0x01).
	// Compact records inherit fixed unit lookups but no per-sensor curve.
	Linear   byte
	Unit1    byte
	Unit2    byte // base unit code (§43.17)
	Unit3    byte // modifier unit code (rarely used)
	M        int16
	B        int16
	Accuracy int16
	Rexp     int8 // K2 — result exponent
	Bexp     int8 // K1 — B exponent
	Analog   byte // analog data format: 0=unsigned, 1=1's compl, 2=2's compl, 3=non-numeric

	// Thresholds (raw bytes; convert with (s *Sensor).Convert). Populated
	// from the SDR body for Full records; Compact records leave them zero.
	UpperNonRecover byte
	UpperCritical   byte
	UpperNonCrit    byte
	LowerNonRecover byte
	LowerCritical   byte
	LowerNonCrit    byte
	ThreshMask      uint16 // upper12 mask from byte 19-20; used to decide which fields are valid
}

// GetSDRRepositoryInfo runs the Get SDR Repository Info command (0x0A/0x20).
func (c *Client) GetSDRRepositoryInfo() (SDRRepoInfo, error) {
	resp, err := c.Send(NetFnStorage, cmdGetSDRRepoInfo, nil)
	if err != nil {
		return SDRRepoInfo{}, err
	}
	if len(resp) < 15 || resp[0] != 0 {
		return SDRRepoInfo{}, fmt.Errorf("ipmi: get sdr repo info: cc=0x%02x len=%d", ccOrZero(resp), len(resp))
	}
	return SDRRepoInfo{
		Version:     resp[1],
		RecordCount: binary.LittleEndian.Uint16(resp[2:4]),
		FreeSpace:   binary.LittleEndian.Uint16(resp[4:6]),
	}, nil
}

// ReserveSDRRepository runs Reserve SDR Repository (0x0A/0x22).
func (c *Client) ReserveSDRRepository() (uint16, error) {
	resp, err := c.Send(NetFnStorage, cmdReserveSDRRepo, nil)
	if err != nil {
		return 0, err
	}
	if len(resp) < 3 || resp[0] != 0 {
		return 0, fmt.Errorf("ipmi: reserve sdr repo: cc=0x%02x", ccOrZero(resp))
	}
	return binary.LittleEndian.Uint16(resp[1:3]), nil
}

// GetSDR runs Get SDR (0x0A/0x23) for a specific record chunk. offset+length
// are bytes into the record (header at 0). length=0xFF means read to the
// end of the record. Returns (nextRecordID, chunk, err); the caller must
// concatenate chunks until it has the header's declared length.
func (c *Client) GetSDR(reservationID, recordID uint16, offset, length byte) (uint16, []byte, error) {
	req := []byte{
		byte(reservationID), byte(reservationID >> 8),
		byte(recordID), byte(recordID >> 8),
		offset, length,
	}
	resp, err := c.Send(NetFnStorage, cmdGetSDR, req)
	if err != nil {
		return 0, nil, err
	}
	if len(resp) < 3 {
		return 0, nil, fmt.Errorf("ipmi: get sdr: short resp %d", len(resp))
	}
	if resp[0] != 0 {
		return 0, nil, fmt.Errorf("ipmi: get sdr: cc=0x%02x", resp[0])
	}
	next := binary.LittleEndian.Uint16(resp[1:3])
	return next, resp[3:], nil
}

// getSDRRecord assembles one full SDR by paging Get SDR. Chunks are 16
// bytes to stay under the 30-byte IPMI-over-LAN payload budget most BMCs
// enforce. Returns the fully assembled record and the "next" record ID.
func (c *Client) getSDRRecord(reservationID, recordID uint16) ([]byte, uint16, error) {
	// First read: pull the 5-byte header so we know the total length.
	next, first, err := c.GetSDR(reservationID, recordID, 0, 5)
	if err != nil {
		return nil, 0, err
	}
	if len(first) < 5 {
		return nil, 0, fmt.Errorf("ipmi: get sdr header short (%d)", len(first))
	}
	total := int(first[4]) + 5
	out := make([]byte, 0, total)
	out = append(out, first...)
	const chunk = 16
	for len(out) < total {
		remain := total - len(out)
		n := chunk
		if remain < n {
			n = remain
		}
		_, more, err := c.GetSDR(reservationID, uint16(recordID), byte(len(out)), byte(n))
		if err != nil {
			// Some BMCs invalidate the reservation mid-walk (cc=0xC5).
			// Callers handle by re-reserving and restarting.
			return nil, 0, err
		}
		if len(more) == 0 {
			break
		}
		out = append(out, more...)
	}
	return out, next, nil
}

// ScanSensors walks the SDR repository and returns every sensor record
// (Type 1 + Type 2) parsed into a Sensor.
func (c *Client) ScanSensors() ([]Sensor, error) {
	resv, err := c.ReserveSDRRepository()
	if err != nil {
		return nil, err
	}
	var out []Sensor
	rid := uint16(0)
	for {
		rec, next, err := c.getSDRRecord(resv, rid)
		if err != nil {
			// Re-reserve once on cc=0xC5 (reservation cancelled).
			if isCC(err, 0xC5) {
				resv, err = c.ReserveSDRRepository()
				if err != nil {
					return out, err
				}
				continue
			}
			return out, err
		}
		if len(rec) >= 5 {
			s, ok := parseSDR(rec)
			if ok {
				out = append(out, s)
			}
		}
		if next == 0xFFFF {
			break
		}
		if next == rid { // loop guard: some BMCs return same ID at EOL
			break
		}
		rid = next
	}
	return out, nil
}

// parseSDR decodes a full SDR record (header + body). Returns ok=false for
// records we skip (not a sensor: type != 0x01/0x02).
func parseSDR(rec []byte) (Sensor, bool) {
	if len(rec) < 6 {
		return Sensor{}, false
	}
	s := Sensor{
		RecordID: binary.LittleEndian.Uint16(rec[0:2]),
		Type:     SDRType(rec[3]),
	}
	body := rec[5:]
	switch s.Type {
	case SDRTypeFull:
		return parseFullSDR(s, body)
	case SDRTypeCompact:
		return parseCompactSDR(s, body)
	}
	return Sensor{}, false
}

// parseFullSDR extracts fields from a Type 1 (Full Sensor) record body.
// Byte offsets are counted from the start of the body (i.e., IPMI spec
// offset 6 = body[0]). See §43.1.
func parseFullSDR(s Sensor, b []byte) (Sensor, bool) {
	if len(b) < 43 { // body must cover through the ID string type/length byte
		return s, false
	}
	s.OwnerID = b[0]
	s.OwnerLUN = b[1] & 0x03
	s.Number = b[2]
	s.EntityID = b[3]
	s.EntityInst = b[4] & 0x7F
	// b[5]..b[8] are sensor initialisation + capabilities — skipped.
	s.SensorType = b[7]
	s.EventType = b[8]
	// Assertion/deassertion event masks live at b[9..14]; we don't use them.
	// Threshold-readable mask at b[13..14] (upper12): used to know which
	// threshold bytes below carry valid data.
	s.ThreshMask = binary.LittleEndian.Uint16(b[13:15])
	s.Unit1 = b[15]
	s.Unit2 = b[16]
	s.Unit3 = b[17]
	s.Linear = b[18]
	s.Analog = (s.Unit1 >> 6) & 0x03
	// Linearisation coefficients pack awkwardly across bytes 19-24.
	// See spec Table 43-1 / §43.1 for the exact bit layout.
	mLo := uint16(b[19])
	mHi := uint16(b[20]&0xC0) << 2 // top 2 bits of M go to bit 8..9
	s.M = signExtend10(mLo | mHi)

	bLo := uint16(b[21])
	bHi := uint16(b[22]&0xC0) << 2
	s.B = signExtend10(bLo | bHi)

	// Accuracy: 10 bits split across b[22] (bits 7:6 → acc low) and b[23],
	// with the exponent in b[23] bits 3:2. Only the coarse value is kept.
	accLo := uint16(b[22]&0xC0) >> 6
	accHi := uint16(b[23]) << 2
	s.Accuracy = signExtend12(accLo | accHi)

	// R exp (result) is bits 7:4 and B exp is bits 3:0 of the "R exp / B exp"
	// byte — spec byte 30, i.e. b[24] (body starts at spec byte 6). Reading
	// b[25] instead lands on the accuracy/tolerance field, which made every
	// sensor parse as Bexp=7/Rexp=0 and inflated voltages by 1000x
	// (P12V showed 11716 instead of 11.72). Both fields are signed 4-bit.
	s.Rexp = sign4(b[24] >> 4)
	s.Bexp = sign4(b[24] & 0x0F)

	// Thresholds. Offsets 27..33 in the body correspond to IPMI bytes
	// 32..38 (upper NR, upper C, upper NC, lower NR, lower C, lower NC).
	// The upper6 field of ThreshMask (bits 5:0) tells which are valid.
	s.UpperNonRecover = b[27]
	s.UpperCritical = b[28]
	s.UpperNonCrit = b[29]
	s.LowerNonRecover = b[30]
	s.LowerCritical = b[31]
	s.LowerNonCrit = b[32]

	// ID string at body offset 42+ (spec byte 48). First byte is type/length.
	if len(b) >= 43 {
		s.Name = parseIDString(b[42:])
	}
	return s, true
}

// parseCompactSDR extracts fields from a Type 2 (Compact Sensor) record.
// Compact records omit linearisation and thresholds; they carry only the
// sensor number, unit type, and name (§43.2).
func parseCompactSDR(s Sensor, b []byte) (Sensor, bool) {
	if len(b) < 27 {
		return s, false
	}
	s.OwnerID = b[0]
	s.OwnerLUN = b[1] & 0x03
	s.Number = b[2]
	s.EntityID = b[3]
	s.EntityInst = b[4] & 0x7F
	s.SensorType = b[7]
	s.EventType = b[8]
	s.Unit1 = b[15]
	s.Unit2 = b[16]
	s.Unit3 = b[17]
	s.Analog = (s.Unit1 >> 6) & 0x03
	if len(b) >= 27 {
		s.Name = parseIDString(b[26:])
	}
	return s, true
}

// parseIDString reads an IPMI type/length-encoded string. Bits 7:6 of the
// first byte are the encoding (00=unicode, 01=BCD+, 10=6-bit ASCII,
// 11=8-bit ASCII), bits 5:0 are the length. We treat everything as ASCII
// since real BMCs never send anything else for sensor names.
func parseIDString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	tl := b[0]
	n := int(tl & 0x1F)
	if n == 0 || 1+n > len(b) {
		return ""
	}
	// Strip trailing NULs/spaces the BMC pads with.
	out := make([]byte, 0, n)
	for _, c := range b[1 : 1+n] {
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	// Trim trailing spaces.
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// signExtend10 sign-extends a 10-bit value in the low bits of v.
func signExtend10(v uint16) int16 {
	if v&0x0200 != 0 {
		return int16(v | 0xFC00)
	}
	return int16(v & 0x03FF)
}

// signExtend12 sign-extends a 12-bit value.
func signExtend12(v uint16) int16 {
	if v&0x0800 != 0 {
		return int16(v | 0xF000)
	}
	return int16(v & 0x0FFF)
}

// sign4 sign-extends a 4-bit value.
func sign4(v byte) int8 {
	if v&0x08 != 0 {
		return int8(v | 0xF0)
	}
	return int8(v & 0x0F)
}

// ccOrZero returns resp[0] or 0 if resp is empty. Used only for error text.
func ccOrZero(resp []byte) byte {
	if len(resp) == 0 {
		return 0
	}
	return resp[0]
}

// isCC returns true if err's completion-code text matches. Cheap: we
// format completion codes as "cc=0xXX" in this package, so a substring
// match suffices without adding another error type.
func isCC(err error, cc byte) bool {
	if err == nil {
		return false
	}
	needle := fmt.Sprintf("cc=0x%02x", cc)
	return contains(err.Error(), needle)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
