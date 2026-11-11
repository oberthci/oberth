# Verified plugin bootstrap

`claude-plugin/scripts/fetch-oberth [version]` installs the newest release,
when no version is given, or a requested v-prefixed semantic version. It
requires curl or wget, OpenSSL, and sha256sum or shasum, plus the standard
POSIX shell utilities. Helm and kubectl remain user prerequisites for the
subsequent installation; the fetcher does not install either tool.

The script reads the newest version from
`https://releases.oberth.ci/oberth/latest/VERSION`, then downloads only
immutable objects under `https://releases.oberth.ci/oberth/<version>/`.
It authenticates `SHA256SUMS` using `SHA256SUMS.sig` and the public key
shipped in `claude-plugin/pins/release-cosign.pub`. It does not accept a key
from the network or an environment variable. The key file's SHA-256 pin is
checked before any download. CI's `TestPinnedKeyIsTheReleasePipelinePin`
checks the key bytes and pins in the plugin, release pipeline, and CLI;
this test belongs to the release race-test rest group.

The manifest must contain exactly the four supported CLI binaries and
`oberth-<version without v>.tgz`, without duplicate or unexpected paths.
The fetcher hashes only the selected binary, using its own filename. It
never executes a downloaded file during verification. After verifying, it
stages the binary on the destination filesystem, installs it with mode
0755 at `~/.local/bin/oberth-<version>` (including the leading `v`), and
atomically replaces the `~/.local/bin/oberth` symlink. A concurrent fetch
is refused. A failed download or verification leaves the existing binary
and symlink in place. A killed process can leave `.oberth-fetch-lock` in
that directory; remove the empty directory only after checking that no
fetch is running, then retry.

The plugin commands' `allowed-tools` entries permit only their own
scripts. The install wrapper checks Helm and kubectl, calls the verified
`~/.local/bin/oberth` directly for plan, install and verify, and refuses an
install without an explicit nonempty `--secrets-file`. Other shell operations in the guided install still use Claude
Code's normal permission checks. The Review confirmation remains the
explicit gate for installing into the context the user chose.

## Release verification

The release pipeline packages the chart before signing `SHA256SUMS`.
It takes the raw DER signature from the authenticated Sigstore bundle,
checks the bundle's SHA-256 digest, and verifies the DER signature using
OpenSSL. Before publication and finalization it also compares the binary
and chart digests with `release.json`. Both signatures authenticate the
same checksum file; no private key conversion or export is needed.

For manual verification, obtain `release.pub` from the trusted source
checkout's `.oberth/pins/release-cosign.pub`, and download `SHA256SUMS`,
`SHA256SUMS.sig`, all four binaries and the chart from the same versioned
release directory:

```sh
openssl dgst -sha256 -verify release.pub -signature SHA256SUMS.sig SHA256SUMS
sha256sum -c SHA256SUMS
```

Never substitute a public key downloaded beside the artifacts for the
trusted source pin.
