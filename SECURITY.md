# Security Policy

beefkvm talks to a BMC, and a BMC is total control of a machine: power, boot
device, virtual media, and keyboard input. Please treat issues here accordingly.

## Reporting a vulnerability

Use GitHub's **[private vulnerability reporting](../../security/advisories/new)**
so the report stays confidential until there's a fix. If that isn't available
to you, email **laizhyi@gmail.com** with `beefkvm` in the subject.

Please include what it affects, how to reproduce it, and what an attacker
gains. A proof of concept helps enormously.

This is a small volunteer project, not a funded product: expect an
acknowledgement within about a week. There is no bug bounty. Credit is given in
the advisory and release notes unless you'd rather stay anonymous.

**Never include your BMC credentials, a session token (`?t=…`), or an
unredacted console capture in a report.** See the capture-scrubbing guidance in
[CONTRIBUTING.md](CONTRIBUTING.md#sharing-captures-safely).

## Supported versions

The project is pre-1.0 and moves fast. Only the `main` branch is supported —
fixes land there, and there are no backports.

## Known limitations — by design, not bugs

These are documented in the README's security section and are **not** what the
process above is for. Reporting them isn't a vulnerability report; improving
them is very welcome as a PR.

- **The web UI is plain HTTP** and defaults to `127.0.0.1`. There is no HTTPS,
  no session timeout, and no audit log.
- **Access is gated by one random 256-bit per-run token** carried in the URL
  and then a cookie, plus a same-origin check on the WebSocket. A token in a
  URL is not a strong secret: it can reach shell history, proxy logs, and
  browser history. If you bind to a non-loopback address with `--listen`, put a
  TLS-terminating reverse proxy and real authentication in front of it.
- **Certificate pinning is the only transport authentication** to the BMC. The
  BMC's certificate is self-signed and typically expired, so it cannot be
  validated against a CA. Learn the pin over a network you trust; a mismatch
  aborts the connection.
- **Anyone who reaches the UI controls the machine.** There is no per-user
  authorisation model — it is a single-operator local tool.

## What is in scope

Things that would genuinely surprise an operator, for example:

- Bypassing the session token or the same-origin check.
- Certificate pinning that can be defeated, or a pin mismatch that fails open.
- Injection into the web UI — for instance BMC-supplied text (usernames, FRU
  strings, certificate fields) reaching the DOM unescaped.
- Credentials leaking into logs, error messages, or process arguments.
- Path traversal or arbitrary file read/write through the virtual-media upload
  or the server-side mount path.
- Memory-safety or panic-on-malformed-input bugs in the protocol and codec
  parsers, which handle untrusted data from the BMC.

## A note on your own deployment

If your BMC still has vendor-default credentials, change them before anything
else. That single step matters more than everything in this file.
