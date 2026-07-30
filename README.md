# beefkvm

A browser-based KVM and management console for Avocent-style BMCs, in pure Go.

Named after the `0xBEEF` frame magic in the APCP/AVPT protocol it speaks.

## Why

Some server BMCs ship a remote console that is a **Java applet**. That applet
needs a JRE with `applet` support, TLS 1.0, SSLv3-era ciphers, and legacy
renegotiation. On any current OS and browser it simply does not launch — which
means perfectly good hardware becomes unmanageable, with no vendor fix coming.

`beefkvm` replaces it. It speaks the BMC's wire protocol directly and renders
the console in a normal browser tab:

- **No Java.** No applet, no JNLP, no bundled JRE, no native libraries.
- **No vendor binaries.** Nothing proprietary is redistributed here.
- **One static Go binary** plus a single embedded HTML page.

## What it does

| Area | Status |
|---|---|
| KVM video (custom DCT codec) | Working — full-rate, sharp text |
| Keyboard (USB HID) + auto-repeat | Working |
| Mouse (absolute) | Working |
| Key macros: Ctrl+Alt+Del, Alt+Tab, Ctrl+Alt+F1–F6, sticky modifiers | Working |
| Clipboard paste into the console | Working |
| Virtual media (mount ISO / USB / floppy) | Working |
| Power on / off / reset / soft shutdown / cycle | Working (IPMI) |
| One-time boot device (PXE / CD / HDD / BIOS) | Working (IPMI) |
| Sensors (temps, fans, voltages) + live stream | Working (IPMI SDR) |
| System event log (view + clear) | Working (IPMI SEL) |
| System info: BMC firmware, IPMI version, FRU | Working |
| BMC TLS certificate inspection | Working |
| User management (add / edit / privilege / enable) | Working (IPMI) |
| Network config (read; guarded write) | Working (IPMI LAN) |
| Serial-over-LAN console | Working — see note below |
| Smart card (CAC/PIV) redirection | Plumbed; needs a PC/SC reader to exercise |
| Automatic reconnect on session drop | Working |

### Notes on the last three

- **Serial-over-LAN** activates and carries traffic, but SoL only transports
  what the *target OS* puts on the serial line. If the screen stays blank, the
  target has no serial console. On Linux:
  `systemctl enable --now serial-getty@ttyS0.service` and add
  `console=ttyS0,115200` to the kernel command line.
- **Smart card** redirection is wired end to end (PC/SC on macOS, status in the
  UI, APDU relay over the VM channel), but it has only been exercised as far as
  reader detection — no physical CAC/PIV reader was available. Treat it as
  untested against real cards.
- **Palette video tiles** are implemented and unit-tested, but the BMC used for
  development only ever emits DCT tiles, so that path has not run against live
  hardware.

## Hardware tested

Developed against a single machine, so the compatibility claim is deliberately
narrow:

- ASPEED AST2300-class BMC with embedded Avocent KVM
- BMC firmware **2.44**, APCP server version **2.34**, IPMI **2.0**
- IPMI manufacturer ID 15370 (Avocent/Vertiv)
- Console resolution 1024×768

If it works — or doesn't — on other Avocent-derived BMCs (MergePoint and
various OEM rebadges), please open an issue with the output of
`apcp-probe --host <bmc> --learn-pin` and your BMC firmware version.

## Install

```sh
go install github.com/zheng1/beefkvm/cmd/beefkvm@latest
```

Or build everything from a checkout:

```sh
git clone https://github.com/zheng1/beefkvm
cd beefkvm
go build ./...
```

Go 1.26+. macOS and Linux. The smart-card backend uses cgo + PC/SC on macOS;
elsewhere it compiles to a no-op stub, so `CGO_ENABLED=0` builds are fine.

## Quick start

The BMC's certificate is self-signed (and on many units, long expired), so it
cannot be validated against a CA. `beefkvm` pins it instead. Learn the
fingerprint once:

```sh
apcp-probe --host bmc.example --learn-pin
```

Then start the console:

```sh
beefkvm \
  --host bmc.example \
  --user admin \
  --pass "$BMC_PASS" \
  --pin  "$BMC_PIN" \
  --ipmi-user admin --ipmi-pass "$BMC_PASS"
```

It prints a URL containing a one-time session token. Open it:

```
beefkvm: listening on http://127.0.0.1:8080?t=<token>
```

Credentials are also read from `BMC_HOST`, `BMC_USER`, `BMC_PASS`, `BMC_PIN`,
`BMC_IPMI_USER`, and `BMC_IPMI_PASS`, which keeps them out of your shell
history and process list.

Omit `--ipmi-user`/`--ipmi-pass` to run KVM-only; the power, sensor, event,
user, network, and serial features simply stay unavailable.

## Security posture

Please read this before exposing it to anything.

- **The web UI is plain HTTP and is meant to be local.** It defaults to
  `127.0.0.1`. Access is gated by a random 256-bit per-run token, carried in
  the URL and then a cookie, plus a same-origin check on the WebSocket.
- **A token in a URL is not a strong secret.** It can land in shell history,
  proxy logs, and browser history. If you bind to a LAN address with
  `--listen`, put it behind a TLS-terminating reverse proxy and your own
  authentication. There is no HTTPS, no session timeout, and no audit log yet.
- **Certificate pinning is the only transport authentication.** A pin mismatch
  aborts the connection. Learn the pin over a network you trust.
- **A BMC is total control of the machine** — power, boot device, virtual
  media, and keyboard. Anyone who reaches this UI has all of that. If your BMC
  still has vendor-default credentials, change them before anything else.

## Protocol notes

`docs/apcp-protocol.md` documents the wire format as observed: the AVPT/`BEEF`
framing, session types (KVM, virtual media, and the companion video channel),
the login handshake, and the video packet layout.

The video codec is not baseline JPEG. It is a custom DCT variant with a 4-bit
opcode stream, a 4-entry palette cache, its own quantisation tables, and an
integer AAN IDCT. `internal/video` implements it; the comments there record the
details that actually mattered — quantisation table ordering, tile raster
wrapping, and the per-frame ACK that keeps the stream alive.

## Tools

| Command | Purpose |
|---|---|
| `beefkvm` | The console: web UI + KVM + IPMI bridge |
| `apcp-probe` | Connect, learn the cert pin, inspect the handshake |
| `apcp-capture` | Record raw protocol frames for analysis |
| `beefkvm-vm` | Mount virtual media from the CLI |
| `beefkvm-power` | Power control from the CLI |
| `beefkvm-usb` | USB/HID experiments |

## Tests

```sh
go test ./...
```

Unit tests cover the codec, framing, and IPMI encoding, and run with no
hardware. Tests that need a live BMC are opt-in via environment variables, for
example:

```sh
IKVM_SOL_LIVE=1 SOL_HOST=bmc.example SOL_USER=admin SOL_PASS=secret \
  go test ./internal/ipmi -run TestSOLLive -v
```

Be aware that the live user-management test creates and then removes a
temporary account in an unused slot.

## Legal

This is an independent, clean-room-style reimplementation produced by observing
network traffic and behaviour, for the purpose of **interoperability** with
hardware the user already owns.

- No vendor source code, JARs, native libraries, or firmware is included in or
  distributed by this repository.
- Avocent, MergePoint, Vertiv, ASPEED, and any other names mentioned are
  trademarks of their respective owners, used here only to identify the
  hardware this software talks to. There is no affiliation or endorsement.
- To use this you need your own BMC and your own credentials.

## License

MIT — see [LICENSE](LICENSE).
