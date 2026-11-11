# Verified plugin bootstrap

The plugin runs `scripts/fetch-oberth [vMAJOR.MINOR.PATCH]`. Without a version it
reads the release origin's `oberth/latest/VERSION`, then fetches immutable
versioned objects. It needs POSIX sh, curl or wget, OpenSSL, and sha256sum or
shasum. It uses neither sudo nor Python, jq, cosign, Helm or kubectl.

The plugin carries `pins/release.pub`, equal to the release pipeline and CLI's
compiled-in public key. The CI Go test checks all three pins. The script checks
`SHA256SUMS.sig` with OpenSSL before reading the signed manifest, verifies the
selected binary's checksum, then installs `~/.local/bin/oberth-<version>` and
atomically switches the `oberth` symlink. A failed check leaves the installed
binary untouched. The manifest must contain exactly four platform binaries and
the version's chart; duplicate names, paths and mismatched versions are refused.
The newest-version marker selects a release; the embedded key authenticates it.
A caller that requires a particular version must pass that version explicitly.

Release signing waits for chart packaging. `SHA256SUMS` covers four binaries
and the chart, and cosign signs it with the same pinned key as `release.json`.
`SHA256SUMS.sig` is the raw DER signature extracted from that verified Sigstore
bundle, checked with OpenSSL. Publication extracts it again from the accepted
immutable bundle, so a retry uses the authoritative signature. Public
verification checks the detached signature equals the bundle's signature and
verifies both; the existing Sigstore transparency checks remain required.
The chart is also published beside the binaries for `sha256sum -c SHA256SUMS`.

Manual verification with a separately trusted copy of the release public key:

```sh
openssl dgst -sha256 -verify release.pub -signature SHA256SUMS.sig SHA256SUMS
sha256sum -c SHA256SUMS
```

The install skill grants shell permission only to its bundled fetch and install
scripts. The latter checks user-provided Helm/kubectl prerequisites and calls
the shared CLI plan/install/verify engine. The skill's Review and explicit
“install” consent remain mandatory. The connect skill grants only its trust
script. See [Claude Code skill permissions](https://code.claude.com/docs/en/skills#pre-approve-tools-for-a-skill).
