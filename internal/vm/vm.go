// Package vm implements the client half of the Avocent Virtual Media
// protocol (AVMP) — enough to attach a local ISO file as a virtual CD-ROM
// to the BMC so the target system's BIOS boots from it.
//
// Ported from com.avocent.vm.ApplianceSession. The BMC (server) sends
// small AVMP request messages (VDISK_INFO, DEVICE_STATUS, VDISK_READ,
// VDISK_WRITE, ...) and we respond with populated PacketAVMP frames.
//
// Message-type constants match internal/apcp/msgtype.go.
package vm

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/zheng1/beefkvm/internal/apcp"
)

// Media holds a single mounted CD/floppy image. Only one is supported
// per session for now (drive ID 0).
type Media struct {
	Path       string
	f          *os.File
	blockSize  int
	blocks     int64
	name       []byte // 8.3-ish filename bytes, up to 96 chars
	readOnly   bool
	isCD       bool
	isFloppy   bool
	dirty      bool // true if writes have occurred
	deviceType byte // 0x00=floppy, 0x05=CD-ROM, 0x06=hard disk

	bytesRead    atomic.Uint64
	bytesWritten atomic.Uint64
}

// OpenCD opens path as a CD-ROM image (block size 2048, read-only).
func OpenCD(path string) (*Media, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// name: basename truncated to <=96 bytes
	name := []byte(pathBase(path))
	if len(name) > 96 {
		name = name[:96]
	}
	return &Media{
		Path:       path,
		f:          f,
		blockSize:  2048,
		blocks:     st.Size() / 2048,
		name:       name,
		readOnly:   true,
		isCD:       true,
		deviceType: 0x05, // CD-ROM
	}, nil
}

// OpenUSB opens path as a writable USB drive image (block size 512).
// If create is true and the file doesn't exist, creates a new image of sizeBytes.
func OpenUSB(path string, create bool, sizeBytes int64) (*Media, error) {
	var f *os.File
	var err error
	if create {
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return nil, err
		}
		// Extend to sizeBytes if newly created
		st, _ := f.Stat()
		if st.Size() < sizeBytes {
			if err := f.Truncate(sizeBytes); err != nil {
				f.Close()
				return nil, err
			}
		}
	} else {
		f, err = os.OpenFile(path, os.O_RDWR, 0644)
		if err != nil {
			return nil, err
		}
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	name := []byte(pathBase(path))
	if len(name) > 96 {
		name = name[:96]
	}
	return &Media{
		Path:       path,
		f:          f,
		blockSize:  512,
		blocks:     st.Size() / 512,
		name:       name,
		readOnly:   false,
		isCD:       false,
		deviceType: 0x06, // Hard disk / USB stick
	}, nil
}

// OpenFloppy opens path as a floppy disk image (block size 512, read-only or writable).
// Standard floppy sizes: 1.44MB = 2880 sectors, 720KB = 1440 sectors, 1.2MB = 2400 sectors.
func OpenFloppy(path string, writable bool) (*Media, error) {
	var f *os.File
	var err error
	if writable {
		f, err = os.OpenFile(path, os.O_RDWR, 0644)
	} else {
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	name := []byte(pathBase(path))
	if len(name) > 96 {
		name = name[:96]
	}
	return &Media{
		Path:       path,
		f:          f,
		blockSize:  512,
		blocks:     st.Size() / 512,
		name:       name,
		readOnly:   !writable,
		isFloppy:   true,
		deviceType: 0x00, // Floppy
	}, nil
}

// Blocks returns the number of sectors in the underlying image.
func (m *Media) Blocks() int64 { return m.blocks }

// BlockSize returns the sector size in bytes.
func (m *Media) BlockSize() int { return m.blockSize }

// BytesRead returns the total bytes served in response to VDISK_READ.
func (m *Media) BytesRead() uint64 { return m.bytesRead.Load() }

// BytesWritten returns the total bytes written in response to VDISK_WRITE.
func (m *Media) BytesWritten() uint64 { return m.bytesWritten.Load() }

// Type returns a short string identifying the media class: "cd", "usb", or "floppy".
func (m *Media) Type() string {
	switch {
	case m.isCD:
		return "cd"
	case m.isFloppy:
		return "floppy"
	default:
		return "usb"
	}
}

// Close releases the underlying file.
func (m *Media) Close() error {
	if m.f == nil {
		return nil
	}
	err := m.f.Close()
	m.f = nil
	return err
}

func (m *Media) readSector(startBlock, nBlocks int, buf []byte) (int, error) {
	off := int64(startBlock) * int64(m.blockSize)
	return m.f.ReadAt(buf[:nBlocks*m.blockSize], off)
}

// Session is a VM client session driving the AVMP protocol over an
// authenticated apcp.Session (SessionTypeVM).
type Session struct {
	Conn    io.ReadWriter
	WriteMu *sync.Mutex

	Media *Media

	// VCard holds the smart-card session, if enabled.
	VCard interface {
		HandleVcardXferBlock([]byte) (apcp.Frame, error)
		SendVcardRequest(uint16) apcp.Frame
	}

	log *log.Logger
}

// Run enters the request/response loop until the BMC closes the
// connection or ctx is cancelled. Should be started after apcp.Session
// login succeeds and heartbeat is running.
func (s *Session) Run() error {
	// Announce our virtual disk to the BMC.
	if s.Media != nil {
		if s.Media.isCD {
			if err := s.sendVdiskRequestCD(0, 1); err != nil {
				return err
			}
		} else {
			if err := s.sendVdiskRequestUSB(0); err != nil {
				return err
			}
		}
	}
	// Announce smart-card reader if present.
	if s.VCard != nil {
		frame := s.VCard.SendVcardRequest(0)
		if err := s.sendFrame(frame); err != nil {
			return err
		}
	}
	for {
		f, err := apcp.ReadFrame(s.Conn)
		if err != nil {
			return err
		}
		if err := s.handle(f); err != nil {
			return err
		}
	}
}

// handle dispatches one server-initiated AVMP frame.
func (s *Session) handle(f apcp.Frame) error {
	switch f.Type {
	case apcp.MsgVdiskInfo: // 0x8200: BMC asking about our vdisk
		return s.respondVdiskInfo(f)
	case apcp.MsgDeviceStatus: // 0x8410
		s.debug("← DEVICE_STATUS len=%d", len(f.Payload))
		return nil
	case apcp.MsgVdiskRead: // 0x8300: BMC wants sectors
		return s.respondVdiskRead(f)
	case apcp.MsgVdiskWrite: // 0x8310: BMC wants to write sectors
		return s.respondVdiskWrite(f)
	case apcp.MsgVcardXferBlock: // 0x8301: BMC wants smart-card APDU
		return s.respondVcardXferBlock(f)
	case apcp.MsgVdiskGetAlternateTocData: // 0x8320: BMC wants multi-session TOC
		return s.respondAlternateTocData(f)
	case apcp.MsgDisconnect: // 0x8110
		s.debug("← DISCONNECT reason=%d", f.Payload[0])
		return io.EOF
	case apcp.MsgVdiskRequestRelease: // 0x8140
		s.debug("← VDISK_REQUEST_RELEASE")
		return s.sendVdiskRelease(0)
	case apcp.MsgDeviceConfigurationOption: // 0x8430
		s.debug("← DEVICE_CONFIG_OPTION len=%d", len(f.Payload))
		return nil
	default:
		s.debug("← UNHANDLED type=0x%04x len=%d", f.Type, len(f.Payload))
	}
	return nil
}

// respondVdiskInfo replies to a BMC VDISK_INFO request with our disk's
// metadata.
func (s *Session) respondVdiskInfo(f apcp.Frame) error {
	if s.Media == nil {
		return nil
	}
	return s.sendVdiskRequest(0)
}

// respondVdiskRead honours a BMC request for a range of sectors. Layout
// (from PacketAVMP getDiskStartBlock/getDiskNumberOfBlocks):
//
//	payload[0..1] : disk id (short)
//	payload[2..5] : start block (int)
//	payload[6..9] : number of blocks (int)
//	payload[10..13]: blocking factor (int)
func (s *Session) respondVdiskRead(f apcp.Frame) error {
	if len(f.Payload) < 14 {
		return fmt.Errorf("vm: VDISK_READ payload too short: %d", len(f.Payload))
	}
	diskID := binary.BigEndian.Uint16(f.Payload[0:2])
	startBlk := int(binary.BigEndian.Uint32(f.Payload[2:6]))
	nBlks := int(binary.BigEndian.Uint32(f.Payload[6:10]))
	blkFactor := int(binary.BigEndian.Uint32(f.Payload[10:14]))
	s.debug("← VDISK_READ id=%d start=%d n=%d bf=%d", diskID, startBlk, nBlks, blkFactor)

	if s.Media == nil {
		return s.sendClientStatus(diskID, 2) // error
	}

	// Read in chunks of blkFactor at a time (matches Java loop).
	bytesPerBlock := s.Media.blockSize
	buf := make([]byte, blkFactor*bytesPerBlock)
	remaining := nBlks
	current := startBlk
	for remaining > 0 {
		chunk := remaining
		if chunk > blkFactor {
			chunk = blkFactor
		}
		length := chunk * bytesPerBlock
		off := int64(current) * int64(bytesPerBlock)
		if _, err := s.Media.f.ReadAt(buf[:length], off); err != nil && err != io.EOF {
			s.debug("read: %v", err)
			return s.sendClientStatus(diskID, 2)
		}
		if err := s.sendVdiskReadData(diskID, current, chunk, buf[:length]); err != nil {
			return err
		}
		s.Media.bytesRead.Add(uint64(length))
		remaining -= chunk
		current += chunk
	}
	return s.sendClientStatus(diskID, 0)
}

// respondVdiskWrite honours a BMC write request for writable USB devices.
func (s *Session) respondVdiskWrite(f apcp.Frame) error {
	if len(f.Payload) < 14 {
		return nil
	}
	diskID := binary.BigEndian.Uint16(f.Payload[0:2])

	// If media is read-only, refuse
	if s.Media == nil || s.Media.readOnly {
		return s.sendClientStatus(diskID, 3) // 3 = access-denied
	}

	// Payload layout (from PacketAVMP):
	//   [0..1] : disk id (short)
	//   [2..5] : start block (int)
	//   [6..9] : number of blocks (int)
	//   [10..13]: blocking factor (int)
	//   [14..] : sector data (nBlocks * blockSize bytes)
	startBlk := int(binary.BigEndian.Uint32(f.Payload[2:6]))
	nBlks := int(binary.BigEndian.Uint32(f.Payload[6:10]))
	data := f.Payload[14:]

	expectedLen := nBlks * s.Media.blockSize
	if len(data) < expectedLen {
		s.debug("VDISK_WRITE short data: got %d, need %d", len(data), expectedLen)
		return s.sendClientStatus(diskID, 2) // error
	}

	// Write to backing file
	off := int64(startBlk) * int64(s.Media.blockSize)
	if _, err := s.Media.f.WriteAt(data[:expectedLen], off); err != nil {
		s.debug("write: %v", err)
		return s.sendClientStatus(diskID, 2)
	}

	s.Media.dirty = true
	s.Media.bytesWritten.Add(uint64(expectedLen))
	s.debug("← VDISK_WRITE id=%d start=%d n=%d ✓", diskID, startBlk, nBlks)
	return s.sendClientStatus(diskID, 0) // success
}

// respondVcardXferBlock handles a smart-card APDU request from the BMC.
func (s *Session) respondVcardXferBlock(f apcp.Frame) error {
	if s.VCard == nil {
		s.debug("← VCARD_XFER_BLOCK but no VCard session — ignoring")
		return nil
	}
	resp, err := s.VCard.HandleVcardXferBlock(f.Payload)
	if err != nil {
		s.debug("vcard handle err: %v", err)
		return nil
	}
	return s.sendFrame(resp)
}

// respondAlternateTocData handles a request for multi-session CD TOC.
// Payload layout (request):
//
//	[0..1]: disk id (short BE)
//
// For now, we return an empty TOC (indicating single-session standard ISO).
// Full multi-session support would parse the ISO 9660 volume descriptors.
func (s *Session) respondAlternateTocData(f apcp.Frame) error {
	if len(f.Payload) < 2 {
		return nil
	}
	diskID := binary.BigEndian.Uint16(f.Payload[0:2])
	s.debug("← VDISK_GET_ALTERNATE_TOC_DATA id=%d", diskID)

	// Send empty TOC data (single session)
	// Payload layout (response MsgVdiskAlternateTocData):
	//   [0..1]: disk id (short BE)
	//   [2..5]: TOC data length (int BE)
	//   [6..]: TOC data (empty for single-session)
	body := make([]byte, 6)
	binary.BigEndian.PutUint16(body[0:2], diskID)
	binary.BigEndian.PutUint32(body[2:6], 0) // zero-length TOC = single session
	return s.send(apcp.MsgVdiskAlternateTocData, body)
}

// Detach releases the currently attached media on the given drive ID and
// closes the backing file. The AVMP session itself stays open — callers
// that want to fully tear it down should close the underlying connection.
func (s *Session) Detach(diskID uint16) error {
	if s.Media == nil {
		return nil
	}
	sendErr := s.sendVdiskRelease(diskID)
	closeErr := s.Media.Close()
	s.Media = nil
	if sendErr != nil {
		return sendErr
	}
	return closeErr
}

// sendVdiskRequest sends VDISK_REQUEST (0x0210) offering a disk image.
// Payload layout (PacketAVMP.setVDiskRequestFieldsForCd):
//
//	payload[0..1]: disk id (short)
//	payload[2..5]: sector count (int)
//	payload[6..9]: blocks (int)
//	payload[10]  : read-only flag
//	payload[11]  : min(blocks, 255) — legacy
//	payload[12..13]: name length
//	payload[14..]: name bytes
func (s *Session) sendVdiskRequest(diskID uint16) error {
	nameLen := len(s.Media.name)
	body := make([]byte, 14+nameLen)
	binary.BigEndian.PutUint16(body[0:2], diskID)
	binary.BigEndian.PutUint32(body[2:6], uint32(s.Media.blocks))
	binary.BigEndian.PutUint32(body[6:10], uint32(s.Media.blocks))
	if s.Media.readOnly {
		body[10] = 1
	} else {
		body[10] = 0
	}
	if s.Media.blocks > 255 {
		body[11] = 0xFF
	} else {
		body[11] = byte(s.Media.blocks)
	}
	binary.BigEndian.PutUint16(body[12:14], uint16(nameLen))
	copy(body[14:], s.Media.name)
	return s.send(apcp.MsgVdiskRequest, body)
}

// sendVdiskRequestCD is deprecated, use sendVdiskRequest.
func (s *Session) sendVdiskRequestCD(diskID uint16, numDrives int) error {
	return s.sendVdiskRequest(diskID)
}

// sendVdiskRequestUSB is deprecated, use sendVdiskRequest.
func (s *Session) sendVdiskRequestUSB(diskID uint16) error {
	return s.sendVdiskRequest(diskID)
}

// sendVdiskRelease tells the BMC to unmount the disk.
func (s *Session) sendVdiskRelease(diskID uint16) error {
	body := make([]byte, 2)
	binary.BigEndian.PutUint16(body[0:2], diskID)
	return s.send(apcp.MsgVdiskRelease, body)
}

// sendVdiskReadData sends a chunk of sector data back to the BMC.
// Layout (PacketAVMP.setVDiskReadDataFields):
//
//	payload[0..1] : disk id
//	payload[2..5] : start block
//	payload[6..9] : number of blocks
//	payload[10..] : sector data (nBlocks * blockSize bytes)
func (s *Session) sendVdiskReadData(diskID uint16, startBlk, nBlks int, data []byte) error {
	body := make([]byte, 10+len(data))
	binary.BigEndian.PutUint16(body[0:2], diskID)
	binary.BigEndian.PutUint32(body[2:6], uint32(startBlk))
	binary.BigEndian.PutUint32(body[6:10], uint32(nBlks))
	copy(body[10:], data)
	return s.send(apcp.MsgVdiskReadData, body)
}

// sendClientStatus signals success (0), busy (1), error (2), access-denied (3).
func (s *Session) sendClientStatus(diskID uint16, status uint32) error {
	body := make([]byte, 6)
	binary.BigEndian.PutUint16(body[0:2], diskID)
	binary.BigEndian.PutUint32(body[2:6], status)
	return s.send(apcp.MsgClientStatus, body)
}

// send writes an AVMP frame under the shared write lock.
func (s *Session) send(msgType uint16, body []byte) error {
	frame := apcp.Frame{Type: msgType, Payload: body}
	return s.sendFrame(frame)
}

// sendFrame writes a pre-built frame under the shared write lock.
func (s *Session) sendFrame(frame apcp.Frame) error {
	buf := frame.Encode()
	s.WriteMu.Lock()
	defer s.WriteMu.Unlock()
	_, err := s.Conn.Write(buf)
	return err
}

func (s *Session) debug(fmt string, args ...any) {
	if s.log != nil {
		s.log.Printf(fmt, args...)
	}
}

// SetLogger sets the logger used for debug messages.
func (s *Session) SetLogger(l *log.Logger) { s.log = l }

// pathBase returns the last component of a filesystem path.
func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
