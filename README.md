# Oberth

Oberth is a small, single-node CI service for Kubernetes. It accepts Git over
SSH, runs repository-owned `.oberth/periapsis.go` pipelines as one Kubernetes
Job per push, and exposes concise run, log, promotion, and issue operations over
an authenticated MCP/HTTPS API.

The design is intentionally narrow:

- one long-lived `oberth` Deployment and one `oberth-runner` container per run;
- SQLite, bare Git caches, workspaces, and logs on one PVC mounted at `/data`;
- fixed SSH and HTTPS NodePorts `30022` and `30443`;
- oldest-eligible execution with a default global limit of three Jobs; a
  publication in flight gates only its exact repository/ref while unrelated
  work continues;
- branch pushes receive no Kubernetes Secrets and use the CI cache root;
- reachable tag pushes use release caches and only the repository-declared
  subset of administrator-allowed release Secrets;
- every branch is published after green; explicit promotion green-gates a source,
  merges it locally, tests the merged tree when necessary, and pushes without force;
- one open CI issue per repository and branch, plus five-minute uplink-owned locks.

Accepted receives and upstream publications are durable obligations. The server
records their exact actor, ref, and result before the external side effect, then
performs one bounded startup recovery from Git state after a restart. Promotions
also bind their exact target predecessor; ordinary green branches deliberately
force-sync as required by FAB. SQLite terminal state, CI issue projection, and
publication finalization commit atomically.

Audit actions form a gap-free SHA-256 chain whose heads receive RFC 3161
timestamps and linked Rekor witnesses. Before any Rekor publication, Oberth
creates a deterministic immutable witness intent outside the SQLite PVC. Before
readiness or any mutation, it verifies the complete public witness history and
the intent/completion sequence against SQLite and immutable namespace
ConfigMaps. Its Role can create, get, and list these continuity records but
cannot update, patch, or delete them, so a crash followed by whole-database
rollback cannot hide an accepted Rekor entry. Pinned witnesses use exact Rekor
UUID reads; only the single pending intent is recovered through an authenticated
exact-certificate-and-hash query. An unresolved intent blocks every new mutation
and completes its exact local-chain prefix before any in-flight suffix is
witnessed. Initial continuity loading is paginated with no fixed record-count
lifetime; steady-state checks cache the canonical immutable prefix and exact-read
its current tips. On startup, an existing database and nonempty WAL are copied
into a private read-only inspection snapshot; verification leaves the source
database, WAL, and derived WAL index byte-exact. The source is opened writable
only after the snapshot passes at the exact current schema; the live daemon never
performs an online schema migration. Fresh genesis is allowed only after both complete
immutable continuity lists are empty, and genesis is witnessed before listeners
or application mutation paths start.

There is no workflow-controller integration, reconciliation layer, shadow main
branch, or separate release controller.

## Repository layout

- `cmd/oberth`, `cmd/oberth-runner` — the server and the Job-side runner;
  `cmd/oberth-release-support` and `cmd/oberth-release-image` are built only
  inside release burns.
- `internal/`, `pkg/periapsis` — server internals and the pipeline SDK whose
  exported surface repository pipelines import as `oberth`.
- `charts/oberth` — the Helm chart.
- `.oberth/` — this repository's own `periapsis.go` pipeline, pinned tool
  installer, and release script. Oberth is the CI authority for this
  repository; the hosted upstream is a publication target and carries no
  forge-native CI.
- `website/` — the oberth.ci static site.
- `homebrew/` — the Homebrew formula and its verification script.
- `docs/`, `scripts/`, `hack/` — documentation, the embedded secret-store
  setup script, and contract/verification helpers.

## Local validation

The repository requires the same checks locally and in CI:

```bash
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
```

Run `make build` for the server and runner binaries. Run `make local-flow` for
the executable FAB Example-flow gate: authenticated clone/push, wait timeout,
supersession, red → red → green issue handling, burn-scoped logs, sync,
divergent/fast-forward/already-contained promotion, unreachable-tag rejection,
invalid public-ref rejection with clean restart replay, and a reachable release
burn with immutable tags and masked logs. It uses
temporary Git repositories, SQLite, pinned SSH/TLS identities, and an in-process
runner; Kubernetes Job isolation is checked separately and no container runtime
is required for this gate.

The execution image is not a build environment. It contains the runner plus the
small shell, Git, SSH, TLS, curl, and archive substrate needed to fetch and
install tools. Each repository's setup burns install pinned Go, lint, scan,
chart, signing, and other dependencies into the Job's writable tool directory.
CI and release use separate, per-repository Go caches; neither trust domain can
borrow executable tools from the image or the other cache.

An admitted Oberth tag runs only the `Release` burns in `.oberth/periapsis.go`.
Those burns repeat vet, lint, race, source-scan, and chart gates; build six
reproducible binaries; publish signed immutable R2 objects; repack the exact
server and runner binaries into clean two-platform GAR images; scan, sign, and
fully read those images back; package a chart containing only their digest
references; publish and verify the chart from GAR and public R2; and finally
advance stable binary aliases and the classic Helm index with compare-and-swap.
The release path uses no Docker daemon and never puts a credential on a command
line or in retained logs.

## Install

For source development, render or install the local chart into the dedicated
namespace. A released chart already contains the exact signed server and runner
GAR digests selected by its tag burn:

```bash
helm upgrade --install oberth ./charts/oberth \
  --namespace oberth --create-namespace
```

The public chart repository is `https://charts.cloudtaser.io/oberth`; its
`index.yaml` is advanced only after the corresponding binaries, GAR images, and
chart have passed their remote verification gates. Artifact publication is not
deployment: roll out that released chart through the declared deployment owner.

The chart creates only namespace-scoped RBAC. Its default upstream Secret names
match the restored Oberth estate and `upstream.createSecrets` defaults to false.
For a greenfield install, set `upstream.createSecrets=true`; Helm creates and
retains the empty name-scoped Secrets, the server stays live but not ready, and
`oberth upstream add` performs the confirmed key/host bootstrap. The persistent
SSH host-key and TLS Secrets are generated once when existing names are not
provided and are reused across upgrades.

Both setup commands reach the running daemon through a mode-`0600` in-pod Unix
socket; every SQLite write and bootstrap Secret patch passes the same fail-closed
external audit gate as SSH and MCP mutations. A missing daemon or unavailable
audit proof leaves setup unchanged.

The chart-managed PVC is marked `helm.sh/resource-policy: keep`: uninstalling
the release intentionally leaves SQLite, Git caches, workspaces, and logs
behind. Delete that orphaned PVC only as a separate, explicit data-destruction
operation after a verified backup.

Register the upstream and an uplink from the running pod:

```bash
oberth upstream add github ssh://git@github.com/oberthci
oberth uplink add ~/.ssh/id_ed25519.pub operator@host
```

The uplink command prints the bearer token once and reports the HTTPS
certificate fingerprint. Tokens are stored only as SHA-256 digests.

### Secure MCP connection

Pin the exact leaf certificate reported by `uplink add`; never bypass TLS with
`-k`. The chart certificate is valid for the DNS name `oberth`, so use that name
even when reaching the HTTPS NodePort through a node address:

```bash
export OBERTH_NODE_IP=<node-address>
export OBERTH_HTTPS_PORT=30443
export OBERTH_TLS_FINGERPRINT='SHA256:<value printed by uplink add>'
export OBERTH_TLS_CERT="$PWD/oberth-tls.crt"

kubectl get secret -n oberth oberth-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 --decode >"$OBERTH_TLS_CERT"
actual_fingerprint="$(
  openssl x509 -in "$OBERTH_TLS_CERT" -outform DER |
    openssl dgst -sha256 -binary | openssl base64 -A | tr -d '='
)"
test "$OBERTH_TLS_FINGERPRINT" = "SHA256:$actual_fingerprint"
```

For `tls.existingSecret`, export its `tls.crt` instead. After the fingerprint
comparison, the exact leaf file is the trust anchor and `--resolve` preserves
hostname verification. Put the one-time bearer value in a private header file
so it does not appear in the curl process arguments, then make the first two MCP
JSON-RPC calls:

```bash
oberth_auth="$(mktemp)"
chmod 0600 "$oberth_auth"
read -rsp 'Oberth bearer token: ' oberth_token; printf '\n'
printf 'Authorization: Bearer %s\n' "$oberth_token" >"$oberth_auth"
unset oberth_token

curl --fail-with-body --silent --show-error \
  --cacert "$OBERTH_TLS_CERT" \
  --resolve "oberth:${OBERTH_HTTPS_PORT}:${OBERTH_NODE_IP}" \
  --header "@$oberth_auth" --header 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bootstrap","version":"1"}}}' \
  "https://oberth:${OBERTH_HTTPS_PORT}/mcp"
curl --fail-with-body --silent --show-error \
  --cacert "$OBERTH_TLS_CERT" \
  --resolve "oberth:${OBERTH_HTTPS_PORT}:${OBERTH_NODE_IP}" \
  --header "@$oberth_auth" --header 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  "https://oberth:${OBERTH_HTTPS_PORT}/mcp"

rm -f -- "$oberth_auth"
```

A durable MCP client uses the same URL, bearer header, exact certificate trust
anchor, and hostname mapping. Keep both the token and certificate pin outside
the repository.

`/readyz` checks durable configuration and Kubernetes availability. Transient
upstream reachability is reported separately by `/api/status`, so a VCS outage
is visible without restarting an otherwise healthy control plane.

## Repository pipeline

Repositories keep an interpreted file behind a build constraint:

```go
//go:build ignore

package main

import "oberth"

// The server parses this literal without executing repository code. A release
// Job receives only names that are also in the administrator allowlist.
var ReleaseSecrets = []string{"r2-upload-token", "cosign-secret"}

func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	return oberth.New().
		Retrograde("setup",
			oberth.Step{Name: "install-go", Command: "./.oberth/install-tools.sh", Args: []string{"go"}},
			oberth.Step{Name: "install-lint", Command: "./.oberth/install-tools.sh", Args: []string{"golangci"}},
		).
		Retrograde("lint", ctx.Go.Vet("./..."), ctx.Lint.GolangciLint("./...")).DependsOn("setup").
		Retrograde("test", ctx.Go.TestRace("./...")).DependsOn("lint").
		Prograde("build", ctx.Go.Build("./...")).DependsOn("test").
		Release("release", oberth.Step{Name: "publish", Command: "./.oberth/release.sh"}).
		Build()
}
```

Only the Job-side runner interprets this code. Before a release Job is created,
the server statically parses the exact top-level `ReleaseSecrets` string-slice
literal and snapshots only its intersection with the administrator allowlist;
computed declarations, undeclared credentials, and requests outside the
allowlist fail closed. The server never executes repository-authored Go. The
install script and every tool checksum remain in that repository; a tool merely
present in an execution image is never an implicit pipeline dependency.
