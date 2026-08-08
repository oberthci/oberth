# Oberth Homebrew formula

`Formula/oberth.rb` pins the current released Oberth version and the SHA256
digest of each published platform binary. The binaries themselves live at the
canonical release origin `https://releases.cloudtaser.io/oberth/<tag>/`, next
to a cosign-signed `SHA256SUMS` bundle produced by the repository's release
burns in `.oberth/periapsis.go`.

## Verifying

```sh
sh homebrew/verify-formula.sh
```

Phase 1 checks the formula structure offline (version field, four platform
URLs, four 64-hex digests, HTTPS only). Phase 2 downloads every platform
binary and re-derives its SHA256 independently of the release build.

## Updating

Formula updates are produced by the release pipeline, not by hand: a release
burn bumps the version and digests after the signed artifacts have passed
their remote verification gates. Manual edits bypass that cross-check.

## Installing

This directory is the formula's source of truth inside the monorepo; a
Homebrew tap requires `Formula/` at the tap repository root, so `brew install`
consumes this formula through the published tap rather than this repository
directly.
