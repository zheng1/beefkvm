package ipmi

import (
	"fmt"
)

// ChassisStatus is the decoded response from Get Chassis Status.
type ChassisStatus struct {
	PowerOn           bool
	OverloadFault     bool
	Interlock         bool
	PowerFault        bool
	PowerControlFault bool
	LastEvent         string
	// Raw bytes for debug printing.
	Raw []byte
}

// Chassis control actions per IPMI §28.3.
const (
	ChassisDown  = 0x00 // power down
	ChassisUp    = 0x01 // power up
	ChassisCycle = 0x02 // power cycle
	ChassisReset = 0x03 // hard reset
	// 0x04 = pulse diagnostic interrupt (skipped)
	ChassisSoft = 0x05 // soft shutdown via ACPI
)

// BootDevice values for the "Boot flags" byte, IPMI §28.13 Table 28-14.
type BootDevice byte

const (
	BootNoOverride BootDevice = 0x00
	BootPXE        BootDevice = 0x04
	BootHDD        BootDevice = 0x08
	BootCD         BootDevice = 0x14
	BootBIOS       BootDevice = 0x18
)

// getChassisStatus issues NetFn=Chassis / cmd=0x01 and parses the response.
func (s *Session) getChassisStatus() (*ChassisStatus, error) {
	cc, data, err := s.rawCall(netFnChassisReq, cmdGetChassisStatus, nil)
	if err != nil {
		return nil, err
	}
	if cc != 0 {
		return nil, fmt.Errorf("ipmi: Get Chassis Status cc=0x%02x", cc)
	}
	if len(data) < 3 {
		return nil, fmt.Errorf("ipmi: chassis status too short (%d)", len(data))
	}
	st := &ChassisStatus{
		PowerOn:           data[0]&0x01 != 0,
		OverloadFault:     data[0]&0x02 != 0,
		Interlock:         data[0]&0x04 != 0,
		PowerFault:        data[0]&0x08 != 0,
		PowerControlFault: data[0]&0x10 != 0,
		Raw:               append([]byte{}, data...),
	}
	// Byte 1: last power event (bit 0 = AC failed, bit 1 = overload,
	// bit 2 = interlock, bit 3 = fault, bit 4 = command).
	var reasons []string
	if data[1]&0x01 != 0 {
		reasons = append(reasons, "ac-failed")
	}
	if data[1]&0x02 != 0 {
		reasons = append(reasons, "overload")
	}
	if data[1]&0x04 != 0 {
		reasons = append(reasons, "interlock")
	}
	if data[1]&0x08 != 0 {
		reasons = append(reasons, "fault")
	}
	if data[1]&0x10 != 0 {
		reasons = append(reasons, "ipmi-command")
	}
	if len(reasons) == 0 {
		st.LastEvent = "none"
	} else {
		st.LastEvent = joinComma(reasons)
	}
	return st, nil
}

// chassisControl sends a Chassis Control (0x02) command with the given
// action byte. Response has no data.
func (s *Session) chassisControl(action byte) error {
	cc, _, err := s.rawCall(netFnChassisReq, cmdChassisControl, []byte{action})
	if err != nil {
		return err
	}
	if cc != 0 {
		return fmt.Errorf("ipmi: Chassis Control(%02x) cc=0x%02x", action, cc)
	}
	return nil
}

// setBootDevice programs a one-time boot override via Set System Boot
// Options (parameter 5: Boot Flags). Only the "boot device" bits are set;
// the "valid" bit is asserted so the BMC applies on next boot only.
func (s *Session) setBootDevice(dev BootDevice) error {
	// Set System Boot Options, parameter 5 (Boot Flags). Per §28.13 the boot
	// flags parameter is EXACTLY 5 data bytes — sending 6 (as before) makes the
	// BMC reject with cc=0xC7 "request data length invalid". Wire order:
	//   [0] param selector (bit7 = "set-complete", low7 = param# = 5)
	//   [1] data1: bit7=valid, bit6=persistent, bit5=BIOS/EFI, bits4..0=0
	//   [2] data2: bits 5..2 = boot device selector
	//   [3] data3, [4] data4, [5] data5: options (0)
	body := []byte{
		0x05,      // parameter selector 5 (boot flags)
		0x80,      // data1: valid=1 (apply next boot), persistent=0, EFI=0
		byte(dev), // data2: boot device
		0x00,      // data3
		0x00,      // data4
		0x00,      // data5
	}
	cc, _, err := s.rawCall(netFnChassisReq, cmdSetBootOptions, body)
	if err != nil {
		return err
	}
	if cc != 0 {
		return fmt.Errorf("ipmi: Set Boot Options cc=0x%02x", cc)
	}
	return nil
}

// joinComma is a tiny helper to avoid pulling strings into this file for
// just this one place.
func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
