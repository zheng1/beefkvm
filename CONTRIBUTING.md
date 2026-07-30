# Contributing to beefkvm

Thanks for taking a look. This project exists because a lot of otherwise
healthy server hardware became unmanageable when browsers dropped Java applets,
and the vendor was never going to ship a fix.

## The most useful contribution: a hardware report

beefkvm has been verified end to end on **exactly one machine** (a GIGABYTE
GA-6PXSV4 — see the README). Everything else in the compatibility table is a
hypothesis based on shared code in the vendor's Java clients.

So the single most valuable thing you can do is **tell us whether it works on
your BMC** — especially if it doesn't. Open a
[hardware report](../../issues/new?template=hardware-report.yml) with:

```sh
apcp-probe --host <bmc> --learn-pin
ipmitool -I lanplus -H <bmc> -U <user> -P <pass> -C 3 mc info
```

If video is broken, include the `[avo] tile hdr:` lines from the beefkvm log.
Those bytes carry the packet subtype and codec mode, which is exactly what
determines whether the decoder needs a new path.

**Before you paste anything**, see [Sharing captures safely](#sharing-captures-safely).

## Ground rules for the codebase

**Never commit vendor artifacts.** No Avocent/Vertiv JARs, no native libraries,
no bundled JRE, no firmware images. They are proprietary, redistributing them
would infringe copyright, and beefkvm does not need them to run. `.gitignore`
blocks the obvious names; please don't work around it.

**This is a clean-room-style reimplementation.** Contributions should be based
on observing traffic and behaviour of hardware you own, or on public
documentation (the IPMI spec is public and freely available). Please don't
paste decompiled vendor source into issues, PRs, or comments.

**Claims must match what was actually tested.** The README deliberately
separates "verified working" from "likely to work". If you add a feature you
could not exercise against real hardware, say so plainly in the docs rather
than letting a reader assume it was tested. A feature honestly marked
"untested" is far more useful than one that silently doesn't work.

## Development

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .          # should print nothing
```

Go 1.26+. macOS and Linux. The smart-card backend uses cgo and PC/SC on macOS;
everywhere else it compiles to a stub, so `CGO_ENABLED=0` builds are fine.

### Tests

Unit tests run with no hardware and must stay that way — they cover the video
codec, protocol framing, and IPMI encoding.

Tests that need a live BMC are opt-in through environment variables so `go test
./...` stays green for everyone:

```sh
IKVM_SOL_LIVE=1  SOL_HOST=bmc.example SOL_USER=admin SOL_PASS=secret \
  go test ./internal/ipmi -run TestSOLLive -v

IKVM_USER_LIVE=1 SOL_HOST=... go test ./internal/ipmi -run TestUserProvisionLive -v
IKVM_LAN_LIVE=1  SOL_HOST=... go test ./internal/ipmi -run TestLANLive -v
```

Be aware of what these do to real hardware: the user test creates and then
removes a temporary account in an unused slot, and the LAN test performs a
deliberate no-op write. Don't point them at a machine you can't afford to
disturb.

### If you touch the video decoder

The codec is unforgiving and the failure modes look alike — a wrong
quantisation table, a wrong tile raster, and a wrong IDCT all produce "sort of
an image, but wrong". Some hard-won lessons live in the comments in
`internal/video/`; please keep them there and add to them.

When you change decode behaviour, verify it against a real framebuffer rather
than trusting that it compiles. `/api/screen.png` dumps the current decoded
frame as a PNG, which makes before/after comparison easy.

### Verifying UI changes

The frontend is a single embedded `index.html`. Anything user-visible should be
checked in an actual browser, not just reasoned about — several bugs in this
project's history (a data race producing torn frames, a stale cached page, a
keyboard auto-repeat that never fired) were invisible until the real page ran.

Values that come from the BMC — usernames, FRU strings, certificate fields,
sensor names — must be HTML-escaped before they reach `innerHTML`. Use the
existing `escapeHTML()` helper; a crafted value is otherwise stored XSS.

## Sharing captures safely

Protocol captures and screenshots contain **whatever was on the console**:
hostnames, IP addresses, sometimes credentials being typed. Before attaching
anything to a public issue:

- Scrub or crop hostnames, internal IPs, and serial numbers.
- Never include your BMC password, and never include a beefkvm session token
  (the `?t=…` value) — treat it as a live credential.
- Prefer the packet header bytes (`[avo] tile hdr:`) over full screen captures;
  they are usually all that's needed to diagnose a codec problem.

Maintainers will never ask you for your BMC credentials.

## Pull requests

- Keep changes focused; unrelated refactoring makes review harder.
- Match the surrounding style. Comments here explain *why*, especially where
  the protocol is counterintuitive — that context is the most valuable thing in
  the codebase.
- Make sure `go build ./...`, `go vet ./...`, `go test ./...`, and `gofmt -l .`
  are all clean. CI checks these on Linux and macOS.
- Describe how you tested. "Verified on a Dell R710, iDRAC6 firmware 2.92" is
  worth more than a long description of the diff.

## License

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE), the same terms as the rest of the project.
