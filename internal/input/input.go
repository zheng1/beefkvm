// Package input encodes KVM keyboard and mouse events for the AVPT/BEEF
// stream. Packet IDs come from the decompiled Java client:
//
//	0x0200 (512) — key press/release
//	                payload[0]  = 0
//	                payload[1]  = 0 if pressed, 1 if released
//	                payload[2:4]= USB HID keyboard usage code (big-endian)
//	                payload[4:8]= 0
//	0x0202 (514) — reserved (empty payload)
//	0x0204 (516) — reserved (empty payload)
//	0x0208 (520) — mouse button, payload[0]=1 for engage
//	0x0201 (513) — absolute mouse (position + wheel)
//	0x0209 (521) — relative mouse (delta + wheel)
//
// For absolute/relative payload layout see com.avocent.kvm.b.a.jc/kc:
//
//	payload[0]  = 0
//	payload[1]  = button mask (bit0=L, bit1=R, bit2=M)
//	payload[2:4]= X (big-endian short)
//	payload[4:6]= Y (big-endian short)
//	payload[6:8]= wheel (signed short)
package input

import (
	"encoding/binary"

	"github.com/zheng1/beefkvm/internal/apcp"
)

// Button mask bits.
const (
	ButtonLeft   = 0x01
	ButtonRight  = 0x02
	ButtonMiddle = 0x04
)

// KeyEvent produces a keyboard press/release AVPT frame.
// key is a USB HID keyboard usage code (e.g. 'a'=4, Enter=40, LeftCtrl=224),
// which is what the BMC firmware expects on the wire — verified against the
// Avocent Java client's keymap (com.avocent.kvm.base.c.d), which translates
// AWT KeyEvents to HID usage IDs before sending. The firmware applies its own
// layout, so the value is layout-independent.
// pressed=true for key-down, false for key-up.
func KeyEvent(key uint16, pressed bool) []byte {
	body := make([]byte, 8)
	if !pressed {
		body[1] = 1
	}
	binary.BigEndian.PutUint16(body[2:4], key)
	return apcp.AVPTFrame{ID: 512, Payload: body}.Encode()
}

// MouseAbsolute produces an absolute mouse position + button state frame.
// x,y are in the BMC's video resolution coordinate space. wheel is +1/-1
// for a scroll notch.
func MouseAbsolute(x, y int, buttons byte, wheel int) []byte {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	body := make([]byte, 8)
	body[1] = buttons
	binary.BigEndian.PutUint16(body[2:4], uint16(x))
	binary.BigEndian.PutUint16(body[4:6], uint16(y))
	binary.BigEndian.PutUint16(body[6:8], uint16(int16(wheel)))
	return apcp.AVPTFrame{ID: 513, Payload: body}.Encode()
}

// MouseRelative sends a relative-mode mouse update.
func MouseRelative(dx, dy int, buttons byte, wheel int) []byte {
	body := make([]byte, 8)
	body[1] = buttons
	binary.BigEndian.PutUint16(body[2:4], uint16(int16(dx)))
	binary.BigEndian.PutUint16(body[4:6], uint16(int16(dy)))
	binary.BigEndian.PutUint16(body[6:8], uint16(int16(wheel)))
	return apcp.AVPTFrame{ID: 521, Payload: body}.Encode()
}
