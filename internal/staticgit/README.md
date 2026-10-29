# Re-pin and re-qualify the static Git substrate

`build.env` records the reviewed recipe; `qualified-artifacts.json` records its
architecture-specific output bytes. Version pins do not preserve availability:
Alpine's live release repositories can remove superseded APK revisions. An
unchanged checkout may therefore fail at `apk add` after a security update.
This is expected fail-closed behavior. Do not remove exact versions, use
`--force-broken-world`, skip qualification, or copy arbitrary new output hashes
just to unblock an unrelated branch. Plan a reviewed maintenance re-pin when
that happens; it can be necessary within weeks of a release.

1. **Identify the failed input.** Retain the failing `build-static-git-runtime`
   log, source commit, digest-pinned Alpine builder, target architecture and
   unavailable package/version. Distinguish repository retirement from an
   authentication/network error. The installer deliberately uses `/sbin/apk`
   and checks the pinned apk-tools version; changing PATH is not a repair.

2. **Resolve the complete package closure in a clean, approved builder.** Use
   the same Alpine release and target-platform builder as Dockerfile's
   `git-build` and `.oberth/build.yaml`. Capture package repository identities,
   APK package/checksum provenance and `/lib/apk/db/installed` from each of
   linux/amd64 and linux/arm64. Re-pin every changed/transitive package in
   `APK_PACKAGES`, including compiler/binutils, musl/musl-dev, zlib/zlib-dev,
   apk-tools and their dependencies. No unversioned additions: the clean
   install must resolve the entire recorded closure on both architectures.
   If the base image changes, update its digest consistently in Dockerfile
   and the CI builder and review the admission/runner pins that bind it.
   Keep `ALPINE_VERSION`, `MUSL_VERSION` and `ZLIB_VERSION` consistent with
   the installed records. Retain the checked Git source and zero-fuzz patch
   hashes unless those inputs are intentionally changed and reviewed too.

3. **Build both platform substrates and retain candidate outputs.** Use the
   checked-in Dockerfile/`hack/build-static-git.sh` recipe on approved builders,
   not an improvised compiler or the shared host. Retain the stripped static
   `git`, linked `usr/lib/libc.a` and `usr/lib/libz.a`, installed APK record,
   source/patch hashes, builder digest and logs for each architecture. Require
   the expected ELF machine, no PT_INTERP/DT_NEEDED, and the expected Git
   version on a matching native runtime. Never relabel amd64 bytes as arm64.
   Update the component labels in Dockerfile if their versions change.

4. **Review candidate byte pins before accepting them.** Hash those three
   retained files with SHA-256 and propose the exact `git_sha256`,
   `musl_archive_sha256` and `zlib_archive_sha256` entries in
   `qualified-artifacts.json` for each architecture. Preserve old/new records
   and provenance in the review packet. Updating this file is a trust-input
   change, not evidence of qualification. `hack/static-git-metadata` embeds
   this record and refuses differing Git/archive bytes or APK versions; an
   expected first-build mismatch requires this reviewed re-pin, not disabling
   `VerifyBuild`. Run it again against the candidate bytes after the change.

5. **Keep the required CI gates.** `build-static-git-runtime` runs
   `hack/qualify-static-git-build.sh`: rebuild, byte/package verification,
   SBOM generation, Grype scan and the vulnerable-Git positive control.
   `test-static-git-runtime` must then run the full tagged CLI transport suite
   using the fresh read-only payload under nonroot-static-v1 and Landlock.
   Require UID/GID 65534, no added capabilities, no shell/runner executable
   fallback, host-key mismatch refusal, receive hooks, archive/history,
   early remote exits and cancellation/reaping checks. An unavailable sandbox
   is a failure. Build jobs must continue to depend on this gate. Native
   amd64 behavior is covered by that current gate; do not describe arm64's
   static/byte/scan checks as a native arm64 behavioral run. Retain both
   architecture SBOM/security receipts and existing Go/Trivy checks. A scan
   failure, missing/stale DB, ignored HIGH/CRITICAL finding, or unexpectedly
   passing vulnerable control blocks signing.

6. **Publish and bind the reviewed index, through the owner.** Rebuild both
   scratch substrates, review their runtime file inventory (static Git,
   static Oberth, exact helper alias, CA bundle; no shell/OpenSSH/curl/package
   DB), and assemble the two-platform OCI index. Have the authorized publisher
   publish/read back every blob and verify platform digests and byte hashes.
   Only then update `internal/releaseimage/image.go`'s `ServerSubstrate` index
   digest. Changes to embedded build inputs/artifact pins also change the
   source-built `oberth-release-image` tool: rebuild it with the existing
   publisher recipe and update `.oberth/pins/bootstrap-tools.sha256` through
   the normal reviewed tooling process. Run staticgit/releaseimage tests,
   full branch CI and the release rehearsal. Release assembly must verify
   final-image Git bytes and both architecture C/Go security receipts before
   signing. Published substrate bytes alone are not a server release.

The READY packet must include the old/new recipe, closure/provenance, all three
byte hashes per architecture, OCI index/platform digests, readback verification,
`test-static-git-runtime` run ID, SBOMs/scans/control receipts and review outcome.
Retain the prior signed server release and its substrate for exact rollback.
Workers do not publish, alter grants, or run cluster/VM tests locally; use the
existing approved CI/build path and coordinate publication with the owner.
