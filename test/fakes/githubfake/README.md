# Local GitHub-kind upstream

One process serves the surfaces in [INVENTORY.md](INVENTORY.md): SSH Git,
read-only smart HTTPS Git, deploy-key creation, helper authentication probes,
and seeded release-download bytes. Real Git owns objects and refs. Other state
is in memory; deploy-key IDs start at 1 and creation timestamps are fixed.
There is no outbound forwarding or automatic asset download.

Requires Go 1.26, Git with its HTTP backend, and an SSH client. For host checks,
use the shared
on-disk Go temporary directory and a disk scratch directory in TMPDIR:

```sh
go test -p=2 -buildvcs=false ./test/fakes/githubfake/...
data_dir=$(mktemp -d "${TMPDIR:?set a disk scratch directory}/githubfake.XXXXXX")
go run -p=2 -buildvcs=false ./test/fakes/githubfake/cmd/githubfake \
  -root "$data_dir" -ca-out "$data_dir/ca.pem"
```

Both `-root` and `-ca-out` are required. Use a dedicated absolute data directory;
a final symlink is refused, and existing repositories are never adopted.
The defaults bind HTTPS 8443, control HTTP 8080, and SSH 2222 on loopback.
`-https`, `-control` and `-ssh` change listen addresses. Control always requires
a loopback address and is never exposed through the public HTTPS handler.

The process generates a CA and a server certificate at startup and writes the
public trust bundle to `-ca-out`. HTTPS requires TLS 1.2 or later. Set
`-tls-hosts` to the comma-separated local hostnames/IPs clients will use.
Defaults cover localhost and both loopback address families. Existing TLS
material can be supplied as `-ca-cert`/`-ca-key` to sign a fresh leaf.
Generated CAs and their server certificates are valid for one year.
Private material is never returned or logged.

Default SSH keys cover the three host-key families the reference scanner probes.
`-host-key` accepts comma-separated private-key files for stable pins. Control
state exposes `host_keys`, each with `public_key` and `fingerprint`; install
pins for the exact local hostname and port. Git subprocesses ignore global,
system and inherited Git configuration, including hook/config environment.

## Control and fixture state

Control is intentionally unauthenticated on its isolated loopback listener:

- `POST /_control/seed`: additive repositories, public identities and assets.
- `POST /_control/faults`: one finite fault injection.
- `GET /_control/state`: sorted repositories and real Git refs, public deploy
  keys, asset paths, request history, faults with attempts/remaining counts,
  unexpected count and SSH host pins. Control reads do not enter call history.

Seed JSON:

```json
{
  "repositories": [{"name": "org/oberth", "default_branch": "main"}],
  "authorized_keys": [],
  "assets": [{
    "path": "/org/oberth/releases/download/v1/fixture.bin",
    "bytes": "Zml4dHVyZQo="
  }]
}
```

Shared `authorized_keys` are SSH public lines with access to all seeded repos.
Per-repository keys instead arrive over HTTPS through POST
`/api/v3/repos/org/oberth/keys`, with any nonempty
Authorization header and JSON
`{"title":"Oberth","key":"<public line>","read_only":false}`.
The header is a fixture sentinel, not real token validation. Duplicate deploy
keys return 422. SSH user `git` is supported; repository keys cannot cross repo
boundaries, and read-only keys cannot push. Invisible repos return not-found.

The helper's stored-account auth check uses GET `/api/v3/` for scope headers.
The helper's environment-token login query requires a controlled authentication
precondition in the walk, as in the reference fixtures. Query endpoints and
un-prefixed REST endpoints return 501 and increase `unexpected_count`.

Populate empty repositories through a real Git push to
`ssh://git@localhost:2222/org/oberth.git`. Source-consuming walks must use the
real source plugin and Git transport. HTTPS cloning is read-only at
`https://localhost:8443/org/oberth.git`; point Git's sslCAInfo at the emitted CA.
HTTP receive-pack requests fail as unexpected.

## Faults and accounting

```json
{
  "method": "POST",
  "path": "/api/v3/repos/org/oberth/keys",
  "kind": "secondary_rate_limit",
  "remaining": 3,
  "status": 429
}
```

HTTP methods are GET/POST; use an exact path or `*`, without a query.
Repository/owner names are case-insensitive, matching routing; tag and asset
names retain case. Kinds are `rate_limit`, `secondary_rate_limit`, `not_found`,
`forbidden`, `server_error`, `slow` and `drop`. Throttles default to 403 and
also support 429. Primary supplies remaining/reset headers; secondary supplies
Retry-After. Server errors default to 503 and support 500..599. Slow needs
`delay_ms` in 1..60000 and then completes normally; cancellation ends the wait.
The executable serves HTTP/1.1 only. Drop closes the HTTP or SSH connection
without a response or exit status; it never sends an HTTP error response.
Each fault's read-only `attempts` records consumed
requests; `remaining` specifies consecutive matching requests before recovery.
An old client with two retries needs three consecutive failures to see failure.
Tests prove both one-failure recovery and three-failure exhaustion with curl.

SSH methods are `git-upload-pack` and `git-receive-pack`, with path `org/oberth`.
Kinds: not_found, forbidden, server_error, slow and drop; receive-pack also has
archived and protected. Auth refusal uses method `auth`, path `*`, kind
forbidden. Host-key mismatch uses the wrong pin; lease/tag conflicts use Git.
All unknown routes/commands fail loudly and increase `unexpected_count`.
Harmless LANG/LC_* locale requests are refused quietly. Only public-key
authentication is offered. One authentication-log callback counts wrong-user
failures and attempted methods other than none/publickey. A password-only or
interactive-only client fails after the expected unauthenticated probe because
its method is never offered; these failures add no unexpected calls. Failed
public-key checks for `git` also remain expected, including injected refusals. The normal OpenSSH session-limit notification is also expected.
Assert zero unexpected requests after a clean walk.

## Container and walk wiring

The Dockerfile pins the same Go/Git image digest as the old build workflow.
Only module downloads use build networking; compilation and runtime checks are
offline. The image runs as an unprivileged user with a dedicated data directory.
Set VM_CONTEXT to the VM's configured container context before these commands:

```sh
docker --context "$VM_CONTEXT" build -f test/fakes/githubfake/Dockerfile \
  -t localhost:5001/githubfake:contract .
docker --context "$VM_CONTEXT" push localhost:5001/githubfake:contract
```

The VM registry is `localhost:5001` for publication and
`k3d-rw-registry:5000/` for cluster image references. For later `next-dev` use,
install into namespace `fakes` and expose HTTPS 8443 and SSH 2222 through its
Service. Seed/inspect through a command running beside the process on loopback;
control is not a public Service port. Export the public CA to walk clients and
use a stable host-key file when pins must survive restarts. Register the local
SSH upstream base with its explicit expected fingerprint. Rewrite authored
URLs and host pins to local values, and preload the actual approved dependency
bytes so digest checks continue to test the real source/download clients.

Focused checks use the repository root linter configuration:

```sh
GOTMPDIR="$HOME/.cache/go-tmp" golangci-lint run \
  --config .golangci.yml --concurrency 2 \
  ./test/fakes/githubfake/...
```
