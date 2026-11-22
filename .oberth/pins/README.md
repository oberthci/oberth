The owner creates the signing key pair at the first release and places only the
public PEM at `internal/releaseverify/release-cosign.pub`. The verifier embeds
that single file; its empty unprovisioned contents cannot verify a release.
The private key belongs in the signing step's granted secret, never this tree.

Run `sh hack/release-pin.sh <public-key-file>` to update the canonical PEM and fetcher.
The printed canonical PEM SHA-256 hashes PEM text; use `--replace` for a different key.
`make check` verifies the embedded key and canonical-file hash; an empty pin is unprovisioned.

`base-image.digest` contains the reviewed OCI index digest (`sha256:...`) of a
locally prepared, two-platform base layout. The layout supplies `/usr/bin/git`
with the exact `internal/staticgit` pins, runtime certificates and directories.
Run `hack/base-image/check.sh --base <layout>` offline before accepting a pin.

`base-image.repository` and `base-image.digest` name one public repository and
its exact reviewed two-platform OCI index. Both workflows fetch that pair into
their run's work volume and verify it before building bytes; they use no shared
base cache. Publish and read back a qualified pin before landing it, following
[the base publication commands](../../hack/base-image/README.md).

`go-toolchain` is the exact Go version required by `make build-bytes` and
`make release-bytes`, matching the digest-pinned pipeline build image. It is
checked against `go env GOVERSION`; it does not select a developer's Go toolchain.
`build-bytes` fails if the Go version fixed by the workflows' build-image digest
differs from this pin; that check keeps the two aligned.

`oberth/policy/names` is the private names policy's public secret path in
`.oberth/build.yaml`. Store and grant it to `lint-and-guards-pass` before the
first push; only its guard command receives the delivered file's path.
`oberth/policy/identity` beside it holds the repository identity, one
`Name <email>` line, that every commit's author and committer must match; only
the guard command receives it, as Git configuration.
