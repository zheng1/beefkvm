<!--
Security fixes: please don't open a public PR first — see SECURITY.md.
-->

## What this changes

<!-- A sentence or two. Link the issue it closes, if any. -->

## How it was tested

<!--
This project's habit is to distinguish what was actually verified from what was
only reasoned about. Please say which this is.

  - Hardware you ran it against (model + BMC firmware), or
  - "unit tests only — no hardware available", which is a perfectly fine answer

If you changed the video decoder, a before/after from /api/screen.png is ideal.
If you changed the UI, please confirm you loaded the real page in a browser.
-->

## Checklist

- [ ] `go build ./...`, `go vet ./...`, `go test ./...` pass
- [ ] `gofmt -l .` prints nothing
- [ ] No vendor artifacts (JARs, native libs, firmware, JRE) added
- [ ] No credentials, session tokens, or unscrubbed captures in the diff
- [ ] Docs updated if behaviour changed — and anything untested is labelled as
      untested rather than implied to work
