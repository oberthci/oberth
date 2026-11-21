# Static Git release base

Run this recipe inside a development VM. It compiles Git for
linux/amd64 and linux/arm64 and writes a local OCI layout, not a registry image.
The base contains only `/usr/bin/git`, the certificate bundle and runtime
directories. The product packager adds the server or source-plugin executable.

`inputs.lock.json` pins each architecture's builder image and package archives,
and locates the source archive. `internal/staticgit/build.env` pins the source
and patch hashes. Package installation and compilation run
with Docker networking disabled. The lock mirrors the exact package closure in the
build env; no package index is fetched. The qualification uses the existing
`staticgit.Record.VerifyBuild` to check installed packages and linked archives. Inputs already in the workspace are
checked before reuse, and changed bytes refuse.
Reused package directories refuse any unlisted entry without deleting it. The
install arguments name only the verified package files.

The patch permits the `-p <port>` pair for the simple SSH variant and executes
local Git service commands directly without a shell.

Requirements: Python 3 and Docker Buildx; arm64 emulation from the pinned
`emulation_image`. The recipe builds one architecture at a time.
Keep the verified inputs archive with release evidence; rebuilding without it
requires new pins and a new qualification.

From the source root inside the VM:

```
python3 hack/base-image/build.py /cache
```

Outputs are `/cache/artifacts/{amd64,arm64}`, per-platform build logs,
`/cache/build-report.json`, `/cache/base-image`, and `/cache/candidate.digest`.
The build report compares Git, musl and zlib against the existing reviewed
records. A mismatch requires a separate qualification decision; it never changes
those records or writes the canonical pin.

The assembly stage pins its Python interpreter and compression libraries by
image digest. The OCI tar members, configuration and index use fixed order, ownership,
permissions and timestamps. `/tmp` is mode 1777 and `/data` belongs to 65534.
The certificate bundle is data, not a TLS transport or a shell dependency.

Run the existing checker offline with the candidate pin first:

```
GOPROXY=off GOSUMDB=off hack/base-image/check.sh \
  --base /cache/base-image --pin /cache/candidate.digest
```

Only after the checker and runtime qualification pass, copy the displayed index
digest to `.oberth/pins/base-image.digest`. The recipe and pin receive review
together. Publish the qualified layout and read its digest back as below before
pipelines fetch that pin.

## Runtime qualification

`base_image_qualification` enables container-only integration tests. The
payload uses the real native Git cache, SSH server, SSH shim and receive hook.
Its image has no shell, Python or OpenSSH, and uses the product packager's
actual image function and PATH, HOME, TMPDIR and server entrypoint. Tests cover
client clone/fetch/push, server clone/fetch/publication, exact received refs and
refusal of an untrusted host key before a Git service is reached.

Compile `internal/sshserver`'s tagged tests with `CGO_ENABLED=0` for both targets
and place them at `/cache/qualification/oberth-linux-{amd64,arm64}`. Compile the
tagged `cmd/oberth-release` tests for the VM's native architecture and run only
`TestPackageBaseQualificationImages` with `OBERTH_BASE_QUALIFICATION=/cache`.
This calls the same base verifier and image constructor as product packaging and
writes `qualification/{amd64,arm64}.tar`. Load each tar into the VM's Docker and
run its `oberth-base-qualification:<architecture>` image with no external
network and a bounded test timeout, selecting `TestBaseImage`.

These tests qualify native Git/SSH behavior in the packaged filesystem. Full
command-line composition, installation, cluster behavior and the release walk
remain separate checks. The temporary qualification executable is not shipped
in the base or substituted for the product executable in a release.

## Publish the qualified release base

After qualification and before landing a base pin, publish the reviewed layout
from the approved release environment. Set `QUALIFIED_LAYOUT` to that exact
layout directory. Use the repository and digest in `.oberth/pins/`; the repository
must permit public reads. These are owner operations, separate from pipeline runs:

```sh
go tool -modfile=hack/tools/crane.mod crane push --index "$QUALIFIED_LAYOUT" \
  "$(cat .oberth/pins/base-image.repository):sha256-$(cut -d: -f2 .oberth/pins/base-image.digest)"
go tool -modfile=hack/tools/crane.mod crane digest \
  "$(cat .oberth/pins/base-image.repository)@$(cat .oberth/pins/base-image.digest)"
```

The readback must equal `base-image.digest` before its pin lands. Retain both
commands, output and exit status, the repository/digest pair, qualification
receipts and verified input archive in the release evidence. Publishing authority
stays in that environment; pipeline preparation uses public reads only.

Both authored workflows fetch the exact index and both platforms into each run's
work volume using `hack/prepare-base.sh`. It restores the pinned root index bytes
from the downloaded blob, verifies the layout with `check.sh`, then exposes it to
byte builds. Missing or mismatched inputs refuse; the shared compilation caches
hold no release base, and a prepared base from another run is never a fallback.
The build's test step runs `check.sh --git-out` to write this machine's checked
base Git and names it in `OBERTH_TEST_GIT`, so the tests built on the gittest
fixture use the Git the release ships; tests that call git from PATH keep the
image's Git.
