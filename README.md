# Oberth

AI-native CI for Kubernetes — a self-hosted gate that runs every AI-agent push
as one Kubernetes Job and lets only green reach your forge.

Agents `git push` to Oberth over SSH. Each push runs the repository's own
`.oberth/periapsis.go` pipeline in a single Job — no forge queue, no API rate
limits, feedback at local speed. Green branches sync upstream automatically;
red runs become issues an agent can lock and fix. Every action is attributed
to a durable uplink identity and recorded in an audit chain anchored outside
the box.

Current release: **v0.10.52** · Helm chart at `https://charts.cloudtaser.io/oberth` ·
Website: [oberth.ci](https://oberth.ci)

## What it does

- **Git ingress** — authenticated Git-over-SSH (NodePort `30022`), smart
  protocol only. Oberth fetches and caches upstream repositories on demand;
  force-pushed feature branches are accepted, tags are creation-only.
- **CI** — one Kubernetes Job per push runs `.oberth/periapsis.go`; steps are
  subprocesses with per-step exit codes and named log slices. FIFO queue with
  a concurrency knob (default 3); a newer push to the same branch supersedes
  its own in-flight run.
- **Issues** — a red run files exactly one issue per repository and branch;
  repeated reds update it, green auto-closes it. Five-minute uplink-owned
  locks keep two agents off the same issue.
- **Promotion** — `promote` green-gates an exact SHA, merges with the target
  locally, runs CI on the merged tree when the target diverged, and pushes
  without force. A moved target fails the promotion; records are append-only.
- **Release** — a tag runs the release burn only if its commit is reachable
  from the default branch, with exactly the secrets the repository declares,
  intersected with the administrator allowlist.
- **MCP** — thirteen tools over authenticated HTTPS for AI agents: status by
  branch, single-step logs, long-poll wait, promotion, and the issue queue.
- **Audit** — every mutation joins a gap-free SHA-256 chain; chain heads
  receive RFC 3161 timestamps and public Rekor witnesses, reconciled against
  rollback-external immutable Kubernetes records.

### Release secrets from your secret store — never through etcd

A repository declares release credentials in one literal map in
`.oberth/periapsis.go`:

```go
var SecretStoreSecrets = map[string]string{
	"r2-token": "oberth/data/r2-upload",
}
```

At release admission Oberth fetches every declared path from your OpenBao or
HashiCorp Vault with its own Kubernetes ServiceAccount identity — before the
release Job exists. A missing secret, an unreachable store, or a path outside
the administrator allowlist fails the release immediately, with the exact path
in the error. Values are injected over the Kubernetes exec stream into a
memory-backed volume inside the running release Job
(`$OBERTH_SECRETSTORE_DIR/<name>/<key>`, `0400`, tmpfs) — never a Kubernetes
Secret, never etcd, never node disk — and every value is masked in the live
log stream. Branch builds receive zero release credentials, always.

Setup is one script run where your own `bao`/`vault` CLI is authenticated —
the Oberth binary has no code path that accepts a store admin token — and
`oberth secretstore verify` proves the whole trust chain from inside the pod.

## Quick start

Five commands from a laptop to gated releases:

```bash
# 1 · any Kubernetes — a k3s box qualifies
curl -sfL https://get.k3s.io | sh -s - --disable=traefik

# 2 · a secret store — OpenBao (dev mode: evaluation only)
helm repo add openbao https://openbao.github.io/openbao-helm
helm install openbao openbao/openbao -n openbao \
  --create-namespace --set server.dev.enabled=true

# 3 · point your store at Oberth — run where your bao/vault CLI is authenticated
curl -fsSL https://oberth.ci/setup-secretstore.sh | bash -s -- \
  --address https://openbao.openbao.svc:8200 --namespace oberth

# 4 · Oberth itself
helm repo add oberth https://charts.cloudtaser.io/oberth
helm install oberth oberth/oberth -n oberth \
  --create-namespace --set secretstore.enabled=true

# 5 · bootstrap identity
kubectl exec -n oberth deploy/oberth -- \
  oberth upstream add github ssh://git@github.com/your-org
kubectl exec -i -n oberth deploy/oberth -- \
  oberth uplink add - agent-07@runner < ~/.ssh/id_ed25519.pub
```

The chart has zero prerequisites: the SSH host key and TLS certificate are
generated once into Secrets and reused across upgrades; an init container
prepares the cache directories; the pod stays `0/1 Running` — live, not
ready — until the first upstream is registered. `upstream add` offers to mint
a deploy key and prints it once for registration at your forge. `uplink add`
prints the TLS fingerprint and a bearer token exactly once; wire it into your
MCP client (full walkthrough in [docs/mcp-setup.md](docs/mcp-setup.md)):

```json
{
  "mcpServers": {
    "oberth": {
      "type": "url",
      "url": "https://<node>:30443/mcp",
      "headers": { "Authorization": "Bearer oberth_…" }
    }
  }
}
```

## How it works

1. **Push** — an agent pushes a branch over SSH; the push is attributed to its
   uplink identity and durably recorded before anything else happens.
2. **Run** — Oberth spawns one Kubernetes Job. Inside it, yaegi interprets the
   repository's `periapsis.go`; each step runs as a subprocess with its own
   exit code, timeout, and named log slice. The server never executes
   repository code.
3. **Record** — green publishes the exact branch SHA to the upstream forge;
   red creates or updates the branch's single CI issue. Nothing lives only on
   the box.
4. **Promote** — `promote <sha> <branch>` verifies the SHA is green, merges
   locally, proves the merged tree when the target diverged, and pushes
   without force.
5. **Release** — a tag reachable from the default branch runs the release
   burn: Kubernetes release Secrets are snapshotted immutably, secret-store
   values are fetched pre-flight from your vault and injected into build
   memory, every value is masked in logs, and the proven tag syncs upstream.
   Your own release pipelines take it from there.

## The pipeline is a Go file

`.oberth/periapsis.go` is what workflow YAML always wanted to be: burns and
steps with real exit codes and named logs, in a language you can run locally
before pushing. Tool installation is inline pipeline steps — fetch, pin,
unpack — not a wrapper script. Every command is visible and auditable, each
fetch gets its own named red/green, and version pins live in repository files
next to the code that uses them:

```go
//go:build ignore

package main

import "oberth"

// A release Job receives only these names, intersected with the
// administrator allowlist. Branch CI receives nothing.
var ReleaseSecrets = []string{"r2-upload-token", "cosign-secret"}

// Fetched from your OpenBao/Vault at release admission, delivered to
// build memory only. Statically parsed, never executed.
var SecretStoreSecrets = map[string]string{
	"r2-token": "oberth/data/r2-upload",
}

func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	return oberth.New().
		Retrograde("setup",
			oberth.Step{Name: "fetch-go", Command: "curl", Args: []string{
				"-fsSL", "-o", "/tmp/go.tgz",
				"https://go.dev/dl/go1.26.0.linux-amd64.tar.gz"}},
			oberth.Step{Name: "pin-go", Command: "sha256sum",
				Args: []string{"-c", ".oberth/pins/go.tgz.sha256"}},
			oberth.Step{Name: "unpack-go", Command: "tar",
				Args: []string{"-C", "/tmp/tools", "-xzf", "/tmp/go.tgz"}},
		).
		Retrograde("lint", ctx.Go.Vet("./...")).DependsOn("setup").
		Retrograde("test",
			ctx.Go.TestRace("./...").WithTimeout(45*oberth.Minute),
		).DependsOn("lint").
		Prograde("build", ctx.Go.Build("./cmd/...")).DependsOn("test").
		Release("release", // tag-trigger only
			oberth.Step{Name: "publish", Command: "go",
				Args: []string{"run", "./release"}},
		).
		Build()
}
```

Only the Job-side runner interprets this file — the server never executes
repository-authored Go. Steps resolve commands from their own declared
environment, never from the runner image's ambient `PATH`; a tool merely
present in an execution image is never an implicit dependency.

## MCP tools (13)

| Tool | Description |
|------|-------------|
| `status` | CI status for a SHA or branch, including the failed step |
| `logs` | One named step's log output for a SHA |
| `wait` | Long-poll until a SHA reaches a terminal state |
| `sync` | Park a WIP branch upstream without a green gate (not completion evidence) |
| `promote` | Green-gate a SHA, merge with target branch, push without force |
| `promote_status` | Wait for a promotion record to become terminal |
| `issue_create` | Create a manual issue |
| `issue_get` | Get an issue by ID |
| `issue_update` | Update an issue title and body |
| `issue_close` | Close an issue (history is kept) |
| `issue_delete` | Delete an accidentally created manual issue |
| `issue_list` | List issue IDs and states (pages of 50) |
| `issue_lock` | Acquire or renew a five-minute caller-owned issue lock |

Authenticated `GET` endpoints (`/api/runs`, `/api/repos`, `/api/issues`,
`/api/status`) serve the same state; `/healthz` and `/readyz` are
unauthenticated.

## Architecture

- **One pod, two ports** — SSH on NodePort `30022`, HTTPS/MCP on `30443`
  (TLS 1.3 or newer). No Argo, no CRDs, no controllers, no step containers.
- **One Job per run, one container** — the digest-pinned ~14 MB runner image
  carries no toolchains; repository setup steps install pinned, checksummed
  tools into the Job's own tool directory. yaegi interprets `periapsis.go`
  inside the Job only.
- **PVC for state** — SQLite, bare Git caches, run workspaces, and retained
  logs on one volume, kept across uninstall. hostPath Go caches are split by
  repository *and* trust tier, so a poisoned branch build can never feed bytes
  into a signed release artifact.
- **Audit chain with external witnesses** — gap-free SHA-256 action chain →
  RFC 3161 timestamps from an independent TSA → linked Rekor witnesses under a
  stable identity derived from the SSH host key → immutable Kubernetes
  continuity records the server can create but never modify. A whole-database
  rollback fails closed instead of forgetting.
- **Degraded-mode startup** — if the Rekor witness endpoint is unreachable at
  startup, the pod comes up running-but-not-ready with every mutation gate
  closed and retries recovery with backoff; it does not crash-loop.
  `auditAnchor.rekorCA` / `auditAnchor.tsaCA` pin private CAs for self-hosted
  witnesses, and `auditAnchor.acceptWitnessChainReset` is the one-shot,
  loudly-logged acknowledgment for reinstalls that abandon a published chain.
- **Rebuild and restore** — every identity Secret is generated on first
  install and kept across uninstall (`helm.sh/resource-policy: keep`). To
  adopt restored credentials after a cluster rebuild, set
  `ssh.hostKey.existingSecret`; Helm then neither adopts nor overwrites that
  Secret. A restored host key without restored local state fails closed
  against the published witness chain until the reset acknowledgment above.

## Security

- Job pods run with `automountServiceAccountToken: false`, as non-root 65534,
  all capabilities dropped, read-only root filesystem, seccomp
  `RuntimeDefault`, bounded resources and deadlines.
- **The trigger is the security boundary.** A branch push is a CI burn and
  mounts nothing — no Secrets, no store access, no ServiceAccount token. A tag
  push is a release burn and receives only `ReleaseSecrets` ∩ administrator
  allowlist (statically parsed from the literal declaration, never evaluated)
  plus `SecretStoreSecrets` ∩ allowlisted paths, fetched pre-flight and
  delivered to memory only.
- Secret-store values never appear in etcd, Kubernetes objects, node disk, or
  logs; per-run store tokens are read-only, short-lived, and revoked after
  every fetch. Every fetch is an actor-attributed entry in the audit chain.
- Bearer tokens map 1:1 to uplink SSH-key fingerprints and are stored only as
  SHA-256 digests, displayed exactly once. SSH accepts the two Git smart-
  protocol commands and nothing else — no shell, no PTY, no forwarding.
- RBAC is namespace-scoped with one documented exception: an optional
  `system:auth-delegator` binding so the store validates logins via
  TokenReview. Audit continuity records are create/get/list only.
- The audit chain is tamper-evident — including against the box it runs on —
  not tamper-proof. See [docs/security.md](docs/security.md) for the full
  invariant list.

## Tested on

**k3s, kind, and k0s** — a 187-scenario acceptance matrix covering Git
ingress, the CI lifecycle, promotion (including concurrent races), release
burns, the secret-store battery, MCP, persistence across restarts, and the
security posture, executed end-to-end on all three engines with **zero
product-blocking failures** and identical behavior across engines. The two
resilience findings it produced — degraded-mode startup during a public Rekor
outage and CA-trust knobs for self-hosted witnesses — shipped in v0.10.51.

## Development

```bash
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
make build       # server and runner binaries
make local-flow  # executable FAB Example-flow gate, no cluster required
```

Watch a live run's current burn and step:

```bash
kubectl get pods -n oberth --watch \
  -L oberth.ci/repo,oberth.ci/trigger,oberth.ci/burn,oberth.ci/step
```

Oberth is its own CI authority: this repository carries
[`.oberth/periapsis.go`](.oberth/periapsis.go), and an admitted tag runs the
release burns that build, sign, publish, and read back every artifact.

## Documentation

- [docs/mcp-setup.md](docs/mcp-setup.md) — connect Claude Code or any MCP client
- [docs/architecture.md](docs/architecture.md) — run lifecycle and recovery
- [docs/security.md](docs/security.md) — security invariants
- [docs/secretstore-verification.md](docs/secretstore-verification.md) — three-tier secret-store verification
- [AGENT-CONTRACT.md](AGENT-CONTRACT.md) — the cross-component compatibility contract

## License

Proprietary — see [LICENSE](LICENSE). Use is governed by the Oberth commercial
license agreement. Licensing and alpha-pilot inquiries: hello@oberth.ci.
