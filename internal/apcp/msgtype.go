package apcp

// AVMP message-type constants derived from
// com.avocent.vm.PacketAVMP + observation notes. Names mostly follow the Java
// constants but drop the PACKET_TYPE_ prefix for brevity.
const (
	// Client → Server
	MsgPreemptResponse           uint16 = 0x0120
	MsgGetVdiskInfo              uint16 = 0x0200
	MsgVdiskRequest              uint16 = 0x0210
	MsgVdiskRequest2             uint16 = 0x0211
	MsgVcardRequest              uint16 = 0x0213
	MsgVdiskRelease              uint16 = 0x0220
	MsgVdiskSetEnable            uint16 = 0x0230
	MsgVdiskReadData             uint16 = 0x0300
	MsgVcardDataBlock            uint16 = 0x0301
	MsgVdiskAlternateTocData     uint16 = 0x0320
	MsgHeartbeat                 uint16 = 0x0400
	MsgClientStatus              uint16 = 0x0410
	MsgUSBReset                  uint16 = 0x0420
	MsgClientConfigurationOption uint16 = 0x0430

	// Server → Client
	MsgDisconnect                uint16 = 0x8110
	MsgDisconnectCancel          uint16 = 0x8120
	MsgVdiskInfo                 uint16 = 0x8200
	MsgVdiskRequestRelease       uint16 = 0x8140
	MsgVdiskRead                 uint16 = 0x8300
	MsgVcardXferBlock            uint16 = 0x8301
	MsgVdiskWrite                uint16 = 0x8310
	MsgVdiskGetAlternateTocData  uint16 = 0x8320
	MsgDeviceStatus              uint16 = 0x8410
	MsgDeviceConfigurationOption uint16 = 0x8430
)

var msgTypeName = map[uint16]string{
	MsgPreemptResponse:           "PREEMPT_RESPONSE",
	MsgGetVdiskInfo:              "GET_VDISK_INFO",
	MsgVdiskRequest:              "VDISK_REQUEST",
	MsgVdiskRequest2:             "VDISK_REQUEST2",
	MsgVcardRequest:              "VCARD_REQUEST",
	MsgVdiskRelease:              "VDISK_RELEASE",
	MsgVdiskSetEnable:            "VDISK_SET_ENABLE",
	MsgVdiskReadData:             "VDISK_READ_DATA",
	MsgVcardDataBlock:            "VCARD_DATA_BLOCK",
	MsgVdiskAlternateTocData:     "VDISK_ALTERNATE_TOC_DATA",
	MsgHeartbeat:                 "HEARTBEAT",
	MsgClientStatus:              "CLIENT_STATUS",
	MsgUSBReset:                  "USB_RESET",
	MsgClientConfigurationOption: "CLIENT_CONFIGURATION_OPTION",
	MsgDisconnect:                "DISCONNECT",
	MsgDisconnectCancel:          "DISCONNECT_CANCEL",
	MsgVdiskInfo:                 "VDISK_INFO",
	MsgVdiskRequestRelease:       "VDISK_REQUEST_RELEASE",
	MsgVdiskRead:                 "VDISK_READ",
	MsgVcardXferBlock:            "VCARD_XFER_BLOCK",
	MsgVdiskWrite:                "VDISK_WRITE",
	MsgVdiskGetAlternateTocData:  "VDISK_GET_ALTERNATE_TOC_DATA",
	MsgDeviceStatus:              "DEVICE_STATUS",
	MsgDeviceConfigurationOption: "DEVICE_CONFIGURATION_OPTION",
}

// MsgTypeName returns a human-readable name for a message type, or a hex-
// formatted UNKNOWN placeholder if we haven't catalogued it yet.
func MsgTypeName(t uint16) string {
	if name, ok := msgTypeName[t]; ok {
		return name
	}
	return "UNKNOWN"
}
