# APCP / AVMP / AVPT protocol reference

Living document. Reverse-engineered from `avctKVM.jar` and `avctVM.jar` for
an ASPEED AST2300 BMC (APCP v2.34). See
`docs/superpowers/specs/2026-07-27-apcp-phase1-design.md` for the phase-1
methodology.

## Transport

- TCP port **2068** for both KVM and Virtual Media channels.
- KVM channel upgrades to **TLSv1.0 with `AES256-SHA` (0x0035)**. The BMC's
  TLS stack (Mbedthis-Appweb 2.4.2) refuses modern ClientHellos; the
  compatible shape is `openssl s_client -tls1 -cipher DEFAULT@SECLEVEL=0
  -legacy_renegotiation`. In Go, use utls with `HelloCustom` +
  ClientHelloSpec that contains only `supported_points` and
  `supported_curves` extensions and a 9-cipher list ending in
  TLS_EMPTY_RENEGOTIATION_INFO_SCSV.
- Certificate: expired 2020 self-signed Avocent cert. Pin its SHA-256 on
  first contact via `apcp-probe --learn-pin`.
- VM channel is **plain TCP** — BMC advertises `caps=0x01` (no SSL bit).

## Wire phase 1 — APCP session setup (both channels)

Client → Server (53 bytes, always plaintext):

| Offset | Size | Field |
|-------:|-----:|-------|
| 0      | 4    | magic `APCP` |
| 4      | 4    | frame length (53, big-endian) |
| 8      | 2    | protocol version (0x0100) |
| 10     | 2    | reserved (0) |
| 12     | 1    | session type: 2=VM, 3=KVM, 4=KVM VM subchannel |
| 13     | 1    | field2: 0 for VM, 2 for KVM |
| 14     | 1    | field3: 0 for VM, 34 for KVM |
| 15     | 1    | reserved (0) |
| 16     | 4    | capability request (5) |
| 20     | 1    | client nonce length (32) |
| 21     | 32   | client random |

Server → Client (53 bytes):

| Offset | Size | Field |
|-------:|-----:|-------|
| 0      | 4    | magic `APCP` |
| 4      | 4    | frame length (53) |
| 8      | 2    | status (must be 0x8100) |
| 10     | 2    | reserved |
| 12     | 1    | version major |
| 13     | 1    | version minor |
| 14     | 4    | connection capabilities: 0x04 = SSL supported, 0x01 = plain |
| 18     | 2    | (unused; observed 0) |
| 20     | 1    | server nonce length |
| 21     | 32   | server random |

## Wire phase 2 — TLS upgrade

If capabilities & 0x04, the same TCP socket is upgraded to TLS in place.
Certificate pin is required. See `internal/tlsdial`.

## Wire phase 3a — AVMP login (VM channel)

Client → Server (153 bytes):

| Offset | Size | Field |
|-------:|-----:|-------|
| 0      | 4    | magic `AVMP` (0x41 0x56 0x4D 0x50) |
| 4      | 4    | frame length (153) |
| 8      | 2    | protocol version (0x0100) |
| 10     | 2    | reserved |
| 12     | 1    | username length |
| 13     | 96   | username, zero-padded |
| 109    | 32   | password, zero-padded (plaintext under TLS) |
| 141    | 8    | zeros |
| 149    | 3    | flags 1..3 (0) |
| 152    | 1    | preempt flag |

Server → Client (111 bytes) — LoginStatus:

| Offset | Size | Field |
|-------:|-----:|-------|
| 0      | 4    | magic `AVMP` |
| 4      | 4    | frame length |
| 8      | 2    | status (0x8100) |
| 10     | 2    | reserved |
| 12     | 1    | login status byte (see AVMPStatus below) |
| 13     | 1    | flag bits (bit0 = can-reject-preemption) |
| 14     | 1    | username length |
| 15     | 96   | username echo, zero-padded |

## Wire phase 3b — AVPT login (KVM channel)

The KVM channel uses a completely different envelope, `BEEF` (0x42 0x45 0x45 0x46),
after TLS is established. Header is 8 bytes: magic + packet id (u16) + total
length (u16). All subsequent traffic on the KVM channel uses AVPT/BEEF.

Login packet (client → server):
- packet id: `256` (non-extended) or `258` (extended, appends one byte)
- body layout (208 bytes, or 209 extended):

| Offset | Size | Field |
|-------:|-----:|-------|
| 0      | 1    | username length |
| 1      | 96   | username, zero-padded UTF-8 |
| 97     | 1    | password length |
| 98     | 96   | password, zero-padded UTF-8 |
| 194    | 8    | MAC / auth (zeros) |
| 202    | 1    | protocol major (echo of SessionSetup.VersionMajor) |
| 203    | 1    | protocol minor |
| 204    | 4    | session id — big-endian, client-generated (`(int)(Math.random() * 1e7)`) |
| 208    | 1    | option (only if extended) |

Server login response is AVPT packet id `33536` (basic) or `33541` (extended):
- byte 0    : status byte (0=success, see AVMPStatus below)
- byte 3    : flags — bit0 = has-companion-video-socket, bit1 = preemption-related
- bytes 4-7 : assigned session ID (integer)
- bytes 8-9 : (extended) additional flags
- bytes 10..: username echo

If flags bit0 is set, the client is expected to open a **second TCP+TLS
socket** to the BMC and send a video-sync packet (packet id 1, 8-byte
payload = session-id + secondary-id). If bit0 is clear, video streams on the
existing socket after the client sends video-enable + set-res + initial-frame
opcodes.

## AVMPStatus login status codes

| Code | Name |
|-----:|------|
| 0    | SUCCEEDED |
| 1    | INVALID_USER_NAME |
| 2    | INVALID_PASSWORD |
| 3    | CHANNEL_ACCESS_DENIED |
| 4    | CHANNEL_IN_USE |
| 5    | CHANNEL_NOT_FOUND |
| 6    | SERVER_NOT_AVAILABLE_CAN_PREEMPT |
| 8    | ALL_CHANNELS_IN_USE |
| 11   | CHANNEL_IN_USE_BY_LOCAL_USER_CAN_PREEMPT |
| 22   | NETWORK_AUTH_SERVER_ERROR |
| 23   | INVALID_EXPIRED_CERT_ERROR |
| 52   | PREEMPT_REJECTED |
| 255 (-1) | GENERIC_FAILED |

## AVMP message types (VM channel, post-login)

| Type (hex) | Name |
|-----------:|------|
| 0x0120 | PREEMPT_RESPONSE |
| 0x0200 | GET_VDISK_INFO |
| 0x0210 | VDISK_REQUEST |
| 0x0211 | VDISK_REQUEST2 |
| 0x0213 | VCARD_REQUEST |
| 0x0220 | VDISK_RELEASE |
| 0x0230 | VDISK_SET_ENABLE |
| 0x0300 | VDISK_READ_DATA |
| 0x0301 | VCARD_DATA_BLOCK |
| 0x0320 | VDISK_ALTERNATE_TOC_DATA |
| 0x0400 | HEARTBEAT (client → server, every ≤10s) |
| 0x0410 | CLIENT_STATUS |
| 0x0420 | USB_RESET |
| 0x0430 | CLIENT_CONFIGURATION_OPTION |
| 0x8110 | DISCONNECT (s→c) |
| 0x8120 | DISCONNECT_CANCEL |
| 0x8140 | VDISK_REQUEST_RELEASE |
| 0x8200 | VDISK_INFO |
| 0x8300 | VDISK_READ |
| 0x8301 | VCARD_XFER_BLOCK |
| 0x8310 | VDISK_WRITE |
| 0x8320 | VDISK_GET_ALTERNATE_TOC_DATA |
| 0x8410 | DEVICE_STATUS |
| 0x8430 | DEVICE_CONFIGURATION_OPTION |

## AVPT/BEEF message types (KVM channel)

Header is 8 bytes: 4-byte `BEEF` magic + 2-byte packet id + 2-byte total length.

| ID (dec) | Name |
|--------:|------|
| 1       | VIDEO_INIT (`dc`) — 8B payload = sessionId(int) + secondaryId(int) |
| 256     | LOGIN (basic) |
| 258     | LOGIN (extended) |
| 512     | KEY_EVENT — {0, released?, keyH, keyL, 0,0,0,0} |
| 513     | MOUSE_ABSOLUTE — {0, buttons, xH,xL, yH,yL, wheelH,wheelL} |
| 514     | KEY_RESET (empty) |
| 516     | KEY_RESET2 (empty) |
| 520     | MOUSE_ENABLE — {enable?, 0..0} |
| 521     | MOUSE_RELATIVE |
| 770     | SET_RESOLUTION — {wH,wL, hH,hL, 0..0} |
| 772     | REQUEST_INITIAL_FRAME |
| 782     | VIDEO_ENABLE — {enable?, quality, 0..0} |
| 1024    | HEARTBEAT — 8B zeros |
| 129..138, 34305..34314 | VIDEO_TILE — variable payload |
| 132     | VIDEO_CONNECT_STATUS |
| 33536   | LOGIN_RESPONSE (basic) |
| 33541   | LOGIN_RESPONSE (extended) |

## Video codec

Video is a stream of AVPT tile packets on the KVM channel. Each tile carries
a 12-byte inner header + compressed payload:

| Offset | Size | Field |
|-------:|-----:|-------|
| 0..3   | 4    | (reserved) |
| 4..5   | 2    | tile x (big-endian short) |
| 6..7   | 2    | tile y |
| 8      | 1    | flags (bit 0 = 'e', bit 1 = 'f') |
| 9..11  | 3    | (reserved) |
| 12..   | N    | compressed pixel stream |

The pixel stream is a variable-length opcode format operating on a linear
pixel cursor:

- `0x00` NOOP N — advance cursor by N pixels
- `0x20` REPEAT N — repeat the last-written pixel N times
- `0x40` COPY N — copy N pixels from previous frame at `cursor - width`
- `0x60` PATTERN — 4-pixel run bit-picked from two previous distinct
  pixels; if bit 0x10 set, additional 7-pixel bytes follow
- `0x80+` COLOR — depth-specific pixel decode

Run lengths use a 5-bit chunk VLE: initial byte contributes low 5 bits;
continuation bytes with matching top-3 bits contribute successive 5-bit
groups shifted up. See `internal/video`.

Multiple bit depths exist in the Java code (`com.avocent.kvm.c.c`
through `.i`); only 15-bit (RGB555, class `c`) is implemented in Go so
far. That decoder:

- Reads 2 bytes per COLOR opcode: `(op & 0x7F) << 8 | next_byte`
- Interprets as 5:5:5 RGB, expanded to 8:8:8 via lookup table.

## Session lifetime

- Client must send a HEARTBEAT frame at least every 10s or the BMC drops
  the session (`HeartBeat.MAX_HEARTBEAT_INTERVAL = 10000ms`).
- KVM channel and VM channel are separate BMC sessions; the BMC may allow
  only one of each at a time. `LOGIN_FAILED_CHANNEL_IN_USE` responses will
  clear ~30–60s after a session is torn down.

## Known gaps (Phase 2/3)

- Only 15-bit video decoder is ported. Depths 16, 24, 32 (`c.d..c.i`) are
  still Java-only.
- Whether the BMC actually starts pushing video tiles after our
  video-enable/set-res/initial-frame sequence on the primary socket has
  not been observed in captured traffic — expected next step is to check
  the "companion video socket" code path (`s.x()` when
  `LoginResponse.e() == true`) and open the second TCP+TLS connection.
- VM disk-sector service (`ApplianceSession$ThreadDSDataInput/Output`)
  is not implemented in Go. Necessary to actually mount an ISO.
