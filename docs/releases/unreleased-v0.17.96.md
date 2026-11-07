# Unreleased: v0.17.96

## Release signatures in the public transparency log

Release signing no longer passes `--use-signing-config=false
--tlog-upload=false`. The pinned release key now signs through cosign's
TUF-provided Sigstore signing config, so the `SHA256SUMS`, `release.json` and
chart bundles and the GAR image and chart signatures carry a public Rekor
inclusion proof and an RFC 3161 timestamp. Every release verification,
including the read-back, public download and tokenless runtime checks, now
requires that log entry; `--insecure-ignore-tlog` is gone from
`.oberth/release.sh` and cosign no longer warns about skipped tlog
verification. The runtime check starts each verification from an empty TUF
cache, so the downloaded binary it executes cannot plant a trust root for the
second check. Key handling, read-back verification and digest pinning are
unchanged.

Release steps reach `rekor.sigstore.dev`, `timestamp.sigstore.dev` and
`tuf-repo-cdn.sigstore.dev` over HTTPS. The pipeline egress policy already
allows port 443 and has no host allowlist, so no policy changes.

Releases up to and including v0.17.95 have no log entry. They are still
re-verified by their own tagged `release.sh`; manual checks and
`hack/upgrade-e2e/run.sh --from` add `--insecure-ignore-tlog` only for those
versions. See [release signature verification](../security.md#release-signature-verification).
`oberth upgrade` does not run cosign, so upgrading from v0.17.95 is unaffected.
