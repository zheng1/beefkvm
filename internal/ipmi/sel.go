package ipmi

import (
	"encoding/binary"
	"fmt"
	"time"
)

// SELInfo is the decoded response of Get SEL Info (§31.2).
type SELInfo struct {
	Version     byte
	EntryCount  uint16
	FreeSpace   uint16
	LastAdd     time.Time
	LastErase   time.Time
	Operations  byte
	Overflow    bool
	SupportsDel bool
}

// SELEntry is one parsed system-event-log record. Only Type 0x02 (system
// event) records get the full sensor decode; OEM records (0xC0-0xFF) come
// through with raw event data plus a synthetic description.
type SELEntry struct {
	RecordID    uint16    `json:"record_id"`
	RecordType  byte      `json:"record_type"`
	Time        time.Time `json:"time"`
	Generator   uint16    `json:"generator"`
	EvMRev      byte      `json:"evm_rev"`
	SensorType  byte      `json:"sensor_type"`
	SensorNum   byte      `json:"sensor_num"`
	EventType   byte      `json:"event_type"`
	Direction   string    `json:"direction"` // "assert" / "deassert"
	EventData   [3]byte   `json:"-"`
	Description string    `json:"description"`
	Severity    string    `json:"severity"` // best-effort colour code for UI
}

// GetSELInfo runs Get SEL Info (0x0A/0x40).
func (c *Client) GetSELInfo() (SELInfo, error) {
	resp, err := c.Send(NetFnStorage, cmdGetSELInfo, nil)
	if err != nil {
		return SELInfo{}, err
	}
	if len(resp) < 15 || resp[0] != 0 {
		return SELInfo{}, fmt.Errorf("ipmi: get sel info: cc=0x%02x len=%d", ccOrZero(resp), len(resp))
	}
	inf := SELInfo{
		Version:    resp[1],
		EntryCount: binary.LittleEndian.Uint16(resp[2:4]),
		FreeSpace:  binary.LittleEndian.Uint16(resp[4:6]),
		LastAdd:    ipmiTime(binary.LittleEndian.Uint32(resp[6:10])),
		LastErase:  ipmiTime(binary.LittleEndian.Uint32(resp[10:14])),
		Operations: resp[14],
	}
	inf.Overflow = resp[14]&0x80 != 0
	inf.SupportsDel = resp[14]&0x08 != 0
	return inf, nil
}

// ReserveSEL runs Reserve SEL (0x0A/0x42).
func (c *Client) ReserveSEL() (uint16, error) {
	resp, err := c.Send(NetFnStorage, cmdReserveSEL, nil)
	if err != nil {
		return 0, err
	}
	if len(resp) < 3 || resp[0] != 0 {
		return 0, fmt.Errorf("ipmi: reserve sel: cc=0x%02x", ccOrZero(resp))
	}
	return binary.LittleEndian.Uint16(resp[1:3]), nil
}

// GetSELEntry runs Get SEL Entry (0x0A/0x43) reading the full 16-byte
// record in one shot (IPMI spec guarantees the record fits; no paging).
// Returns the parsed entry and the next record ID.
func (c *Client) GetSELEntry(reservationID, recordID uint16) (SELEntry, uint16, error) {
	req := []byte{
		byte(reservationID), byte(reservationID >> 8),
		byte(recordID), byte(recordID >> 8),
		0x00, 0xFF, // offset=0, length=0xFF (entire record)
	}
	resp, err := c.Send(NetFnStorage, cmdGetSELEntry, req)
	if err != nil {
		return SELEntry{}, 0, err
	}
	if len(resp) < 3 || resp[0] != 0 {
		return SELEntry{}, 0, fmt.Errorf("ipmi: get sel entry %d: cc=0x%02x", recordID, ccOrZero(resp))
	}
	next := binary.LittleEndian.Uint16(resp[1:3])
	data := resp[3:]
	if len(data) < 16 {
		return SELEntry{}, next, fmt.Errorf("ipmi: get sel entry %d: short record %d", recordID, len(data))
	}
	return parseSELRecord(data), next, nil
}

// ScanSEL walks the entire SEL and returns every entry. reservationID is
// obtained internally; caller doesn't need to reserve first.
func (c *Client) ScanSEL(limit int) ([]SELEntry, error) {
	resv, err := c.ReserveSEL()
	if err != nil {
		return nil, err
	}
	var out []SELEntry
	rid := uint16(0)
	for {
		if limit > 0 && len(out) >= limit {
			break
		}
		e, next, err := c.GetSELEntry(resv, rid)
		if err != nil {
			// Reservation cancelled: re-reserve once and restart from
			// where we left off (rid still points at the next unread).
			if isCC(err, 0xC5) {
				resv, err = c.ReserveSEL()
				if err != nil {
					return out, err
				}
				continue
			}
			return out, err
		}
		out = append(out, e)
		if next == 0xFFFF {
			break
		}
		rid = next
	}
	return out, nil
}

// ClearSEL runs Clear SEL (0x0A/0x47). Op code 0xAA=initiate, 0x00=status.
// We always initiate; the caller can poll status separately if needed.
func (c *Client) ClearSEL(reservationID uint16) error {
	req := []byte{
		byte(reservationID), byte(reservationID >> 8),
		'C', 'L', 'R',
		0xAA,
	}
	resp, err := c.Send(NetFnStorage, cmdClearSEL, req)
	if err != nil {
		return err
	}
	if len(resp) < 1 || resp[0] != 0 {
		return fmt.Errorf("ipmi: clear sel: cc=0x%02x", ccOrZero(resp))
	}
	return nil
}

// ipmiTime converts an IPMI 32-bit timestamp (seconds since 1970-01-01
// UTC, or 0xFFFFFFFF = "no timestamp") to time.Time. Values below the
// "pre-init" cutoff of 0x20000000 (~1987) mean "seconds since BMC boot"
// and are returned as-is relative to Unix epoch; the UI can decide how to
// display them.
func ipmiTime(v uint32) time.Time {
	if v == 0xFFFFFFFF {
		return time.Time{}
	}
	return time.Unix(int64(v), 0).UTC()
}

// parseSELRecord decodes a 16-byte SEL record. Format §32.1:
//
//	[0..1]   record id (LSB first)
//	[2]      record type (0x02 = system event)
//	[3..6]   timestamp (unix)
//	[7..8]   generator ID
//	[9]      EvM Rev (0x03 or 0x04)
//	[10]     sensor type
//	[11]     sensor number
//	[12]     event type (bit7 = deassert)
//	[13..15] event data 1/2/3
func parseSELRecord(d []byte) SELEntry {
	e := SELEntry{
		RecordID:   binary.LittleEndian.Uint16(d[0:2]),
		RecordType: d[2],
	}
	if e.RecordType == 0x02 {
		e.Time = ipmiTime(binary.LittleEndian.Uint32(d[3:7]))
		e.Generator = binary.LittleEndian.Uint16(d[7:9])
		e.EvMRev = d[9]
		e.SensorType = d[10]
		e.SensorNum = d[11]
		et := d[12]
		if et&0x80 != 0 {
			e.Direction = "deassert"
		} else {
			e.Direction = "assert"
		}
		e.EventType = et & 0x7F
		e.EventData[0] = d[13]
		e.EventData[1] = d[14]
		e.EventData[2] = d[15]
		e.Description = describeEvent(e)
		e.Severity = severityFromEvent(e)
	} else {
		// Timestamped OEM (0xC0-0xDF): bytes 3..6 timestamp, 7..15 raw.
		// Non-timestamped OEM (0xE0-0xFF): bytes 3..15 raw.
		if e.RecordType >= 0xC0 && e.RecordType <= 0xDF {
			e.Time = ipmiTime(binary.LittleEndian.Uint32(d[3:7]))
		}
		e.Description = fmt.Sprintf("OEM record type 0x%02X: % x", e.RecordType, d[3:])
		e.Severity = "info"
	}
	return e
}

// describeEvent turns an IPMI event into a short English string. Full
// decode of Table 42-1 / 42-2 is huge; we handle the common threshold
// crossings and fall back to "sensor N event 0xNN" for the rest so the
// operator at least sees something meaningful.
func describeEvent(e SELEntry) string {
	sname := sensorTypeName(e.SensorType)
	dir := e.Direction

	// Threshold event (event type 0x01): event data 1 low nibble tells
	// which threshold was crossed (§29.7 offset table).
	if e.EventType == 0x01 {
		offset := e.EventData[0] & 0x0F
		var what string
		switch offset {
		case 0x00:
			what = "lower non-critical, going low"
		case 0x01:
			what = "lower non-critical, going high"
		case 0x02:
			what = "lower critical, going low"
		case 0x03:
			what = "lower critical, going high"
		case 0x04:
			what = "lower non-recoverable, going low"
		case 0x05:
			what = "lower non-recoverable, going high"
		case 0x06:
			what = "upper non-critical, going low"
		case 0x07:
			what = "upper non-critical, going high"
		case 0x08:
			what = "upper critical, going low"
		case 0x09:
			what = "upper critical, going high"
		case 0x0A:
			what = "upper non-recoverable, going low"
		case 0x0B:
			what = "upper non-recoverable, going high"
		default:
			what = fmt.Sprintf("threshold offset 0x%X", offset)
		}
		return fmt.Sprintf("%s sensor #%d: %s (%s)", sname, e.SensorNum, what, dir)
	}
	// Generic discrete (event type 0x02-0x0C) or sensor-specific (0x6F).
	return fmt.Sprintf("%s sensor #%d: event type 0x%02X offset 0x%X (%s)",
		sname, e.SensorNum, e.EventType, e.EventData[0]&0x0F, dir)
}

// severityFromEvent picks a UI severity from the threshold offset. Non-
// threshold events default to "info" — a stricter caller can override.
func severityFromEvent(e SELEntry) string {
	if e.EventType != 0x01 {
		return "info"
	}
	offset := e.EventData[0] & 0x0F
	switch offset {
	case 0x04, 0x05, 0x0A, 0x0B:
		return "unrecoverable"
	case 0x02, 0x03, 0x08, 0x09:
		return "critical"
	case 0x00, 0x01, 0x06, 0x07:
		return "non-critical"
	}
	return "info"
}
