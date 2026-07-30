package ipmi

import (
	"fmt"
	"math"
)

// Reading is the human-consumable snapshot of one sensor: name, converted
// value in engineering units, textual state, and (optional) thresholds.
type Reading struct {
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	State     string  `json:"state"` // "nominal", "non-critical", "critical", "unrecoverable", "n/a"
	Type      string  `json:"type"`  // "temperature", "voltage", "fan", "power", ...
	Number    byte    `json:"number"`
	LowerNC   float64 `json:"lower_nc,omitempty"`
	LowerCrit float64 `json:"lower_crit,omitempty"`
	LowerNR   float64 `json:"lower_nr,omitempty"`
	UpperNC   float64 `json:"upper_nc,omitempty"`
	UpperCrit float64 `json:"upper_crit,omitempty"`
	UpperNR   float64 `json:"upper_nr,omitempty"`
	HasThresh bool    `json:"has_thresh"`
	Available bool    `json:"available"` // false when the sensor read errored / disabled
}

// GetSensorReading runs Get Sensor Reading (0x04/0x2D). Returns the raw
// value byte, the reading availability flag byte, and the threshold
// comparison mask (§35.14). If the sensor is disabled or its reading is
// unavailable, ok=false.
func (c *Client) GetSensorReading(sensorID byte) (raw byte, ok bool, thresh byte, err error) {
	resp, err := c.Send(NetFnSensor, cmdGetSensorRead, []byte{sensorID})
	if err != nil {
		return 0, false, 0, err
	}
	if len(resp) < 3 {
		return 0, false, 0, fmt.Errorf("ipmi: get sensor reading %d: short resp %d", sensorID, len(resp))
	}
	if resp[0] != 0 {
		return 0, false, 0, fmt.Errorf("ipmi: get sensor reading %d: cc=0x%02x", sensorID, resp[0])
	}
	raw = resp[1]
	avail := resp[2]
	// bit 5 = "reading/state unavailable"; bit 6 = "scanning disabled";
	// bit 7 = "event messages disabled". If either of the first two is
	// set the raw byte is meaningless.
	if avail&(1<<5) != 0 || avail&(1<<6) == 0 {
		// bit 6 is "sensor scanning enabled" — active-high per spec.
		// Some BMCs report 0 here even when data is valid, so we only
		// treat bit 5 as authoritative for "unavailable".
	}
	if avail&(1<<5) != 0 {
		return raw, false, 0, nil
	}
	if len(resp) >= 4 {
		thresh = resp[3]
	}
	return raw, true, thresh, nil
}

// Convert applies the SDR's linearisation formula to a raw byte reading
// per IPMI §36.3:
//
//	y = L((M * x + B * 10^Bexp) * 10^Rexp)
//
// where L is one of the linearisation functions selected by Sensor.Linear.
// Returns 0 for non-analog sensors (discrete).
func (s *Sensor) Convert(raw byte) float64 {
	if s.Analog == 3 {
		return 0 // non-numeric / discrete: no engineering value
	}
	// Decode raw byte per analog data format.
	var x float64
	switch s.Analog {
	case 0: // unsigned
		x = float64(raw)
	case 1: // 1's complement
		v := int16(int8(raw))
		if raw&0x80 != 0 {
			v++ // convert 1's compl to signed
		}
		x = float64(v)
	case 2: // 2's complement
		x = float64(int8(raw))
	default:
		x = float64(raw)
	}
	m := float64(s.M)
	b := float64(s.B)
	k1 := math.Pow10(int(s.Bexp))
	k2 := math.Pow10(int(s.Rexp))
	y := (m*x + b*k1) * k2
	return linearise(s.Linear, y)
}

// linearise applies the SDR's L() function to a converted value.
// Linear=0 is the identity; anything past 7 is either non-linear (0x70,
// looked up via a separate command we don't send) or reserved — we return
// the raw value unmodified in those cases.
func linearise(l byte, y float64) float64 {
	switch l {
	case 0: // linear
		return y
	case 1: // ln
		return math.Log(y)
	case 2: // log10
		return math.Log10(y)
	case 3: // log2
		return math.Log2(y)
	case 4: // e^x
		return math.Exp(y)
	case 5: // 10^x
		return math.Pow(10, y)
	case 6: // 2^x
		return math.Pow(2, y)
	case 7: // 1/x
		if y == 0 {
			return 0
		}
		return 1 / y
	case 8: // sqr
		return y * y
	case 9: // cube
		return y * y * y
	case 10: // sqrt
		return math.Sqrt(y)
	case 11: // cube root
		return math.Cbrt(y)
	}
	return y
}

// State decodes the threshold comparison mask returned by Get Sensor
// Reading into a spec severity string. Bits (§35.14):
//
//	0x01 = at or below lower non-critical
//	0x02 = at or below lower critical
//	0x04 = at or below lower non-recoverable
//	0x08 = at or above upper non-critical
//	0x10 = at or above upper critical
//	0x20 = at or above upper non-recoverable
func stateFromMask(m byte) string {
	if m&(0x04|0x20) != 0 {
		return "unrecoverable"
	}
	if m&(0x02|0x10) != 0 {
		return "critical"
	}
	if m&(0x01|0x08) != 0 {
		return "non-critical"
	}
	return "nominal"
}

// ReadAll fetches Get Sensor Reading for every sensor in the given list
// and returns Reading snapshots. Sensors whose read returns "unavailable"
// are still included with Available=false and Value=0 so the UI can grey
// out the tile rather than have it vanish.
func (c *Client) ReadAll(sensors []Sensor) []Reading {
	out := make([]Reading, 0, len(sensors))
	for i := range sensors {
		s := &sensors[i]
		if s.Analog == 3 || s.EventType != 0x01 {
			// Skip discrete / event-only sensors for the analog dashboard.
			// (SEL viewer picks them up separately.)
			continue
		}
		r := Reading{
			Name:   s.Name,
			Unit:   unitLabel(s.Unit2),
			Type:   sensorTypeName(s.SensorType),
			Number: s.Number,
		}
		raw, ok, mask, err := c.GetSensorReading(s.Number)
		if err != nil || !ok {
			r.State = "n/a"
			r.Available = false
			out = append(out, r)
			continue
		}
		r.Value = s.Convert(raw)
		r.State = stateFromMask(mask)
		r.Available = true

		// Fill thresholds if the SDR flagged them readable.
		if s.Type == SDRTypeFull {
			m := s.ThreshMask
			if m&0x0001 != 0 {
				r.LowerNC = s.Convert(s.LowerNonCrit)
				r.HasThresh = true
			}
			if m&0x0002 != 0 {
				r.LowerCrit = s.Convert(s.LowerCritical)
				r.HasThresh = true
			}
			if m&0x0004 != 0 {
				r.LowerNR = s.Convert(s.LowerNonRecover)
				r.HasThresh = true
			}
			if m&0x0008 != 0 {
				r.UpperNC = s.Convert(s.UpperNonCrit)
				r.HasThresh = true
			}
			if m&0x0010 != 0 {
				r.UpperCrit = s.Convert(s.UpperCritical)
				r.HasThresh = true
			}
			if m&0x0020 != 0 {
				r.UpperNR = s.Convert(s.UpperNonRecover)
				r.HasThresh = true
			}
		}
		out = append(out, r)
	}
	return out
}

// unitLabel maps an IPMI base-unit code (§43.17) to a UI-facing label.
// Only the codes we've seen on real BMCs get short labels; the rest fall
// through to a numeric placeholder so it's at least visible.
func unitLabel(u byte) string {
	switch u {
	case 0:
		return ""
	case 1:
		return "°C"
	case 2:
		return "°F"
	case 3:
		return "K"
	case 4:
		return "V"
	case 5:
		return "A"
	case 6:
		return "W"
	case 7:
		return "J"
	case 8:
		return "C" // coulombs (rare)
	case 9:
		return "VA"
	case 10:
		return "Nits"
	case 11:
		return "lm"
	case 18:
		return "RPM"
	case 19:
		return "Hz"
	case 20:
		return "µs"
	case 21:
		return "ms"
	case 22:
		return "s"
	case 23:
		return "min"
	case 24:
		return "hr"
	case 25:
		return "day"
	case 26:
		return "wk"
	case 27:
		return "mil"
	case 28:
		return "in"
	case 29:
		return "ft"
	case 32:
		return "m"
	case 33:
		return "cm"
	case 34:
		return "mm"
	case 65:
		return "%"
	case 66:
		return "psi"
	case 71:
		return "bit"
	case 72:
		return "kbit"
	case 73:
		return "Mbit"
	case 74:
		return "Gbit"
	case 75:
		return "B"
	case 76:
		return "KB"
	case 77:
		return "MB"
	case 78:
		return "GB"
	}
	return fmt.Sprintf("u%d", u)
}

// sensorTypeName maps a sensor-type code (§42.2 Table 42-3) to a
// dashboard group. We collapse the long table into the buckets the UI
// actually shows: temperature, voltage, current, fan, power, other.
func sensorTypeName(t byte) string {
	switch t {
	case 0x01:
		return "temperature"
	case 0x02:
		return "voltage"
	case 0x03:
		return "current"
	case 0x04:
		return "fan"
	case 0x08:
		return "power" // Power Supply
	case 0x09:
		return "power" // Power Unit
	case 0x0A:
		return "cooling" // Cooling Device
	case 0x0B:
		return "power" // Other Units-Based
	case 0x14:
		return "chipset"
	case 0x15:
		return "board"
	case 0x16:
		return "cpu"
	case 0x17:
		return "chassis"
	}
	return "other"
}
