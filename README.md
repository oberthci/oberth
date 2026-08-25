# Oberth

AI-native CI for Kubernetes — a self-hosted gate that runs every AI-agent push
as one Kubernetes Job and lets only green reach your forge.

Agents `git push` to Oberth over SSH. Each push runs the repository's own
pipeline as an Argo Workflow — no forge queue, no API rate limits, feedback at
local speed. Green branches sync upstream automatically; red runs become
issues an agent can lock and fix. Every action is attributed to a durable
uplink identity and recorded in a tamper-evident audit chain — optionally
anchored outside the box.

Current release: **v0.12.15** · Helm chart at `https://charts.cloudtaser.io/oberth` ·
Website: [oberth.ci](https://oberth.ci)

## What it does

- **Git ingress** — authenticated Git-over-SSH (NodePort `30022`), smart
  protocol only. Oberth fetches and caches upstream repositories on demand;
  force-pushed feature branches are accepted, tags are creation-only.
- **CI** — each push submits the repository's Argo Workflow YAML
  (`.oberth/build.yaml` for branches, `.oberth/release.yaml` for tags); each
  template runs in its own container with its own exit code and named log
  slice. FIFO queue with a concurrency knob (default 3); a newer push to the
  same branch supersedes its own in-flight run.
- **Issues** — a red run files exactly one issue per repository and branch;
  repeated reds update it, green auto-closes it. Five-minute uplink-owned
  locks keep two agents off the same issue.
- **Promotion** — `promote` green-gates an exact SHA, merges with the target
  locally, runs CI on the merged tree when the target diverged, and pushes
  without force. A moved target fails the promotion; records are append-only.
- **Release** — a tag runs the release burn only if its commit is reachable
  from the default branch, with exactly the secrets the repository declares,
  intersected with the administrator allowlist. After every burn is green,
  Oberth publishes the exact admitted tag object upstream.
- **MCP** — eighteen tools over authenticated HTTPS for AI agents: SHA- and
  exact-run status/logs, long-poll wait, trusted plan/apply, promotion, and the
  issue queue.
- **Audit** — every mutation joins a gap-free SHA-256 chain, always. Opt-in
  external anchoring adds RFC 3161 timestamps and public Rekor witnesses,
  reconciled against rollback-external immutable Kubernetes records.

### Release secrets from your secret store — never through etcd

A repository declares release credential paths in an annotation on its
`.oberth/release.yaml` Workflow:

```yaml
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/data/release/r2-upload-token,oberth/data/release/cosign-secret
```

Release steps wrap their command with `envconsul`, which authenticates to
OpenBao via Kubernetes auth using the release-tier ServiceAccount token, reads
the declared paths, and injects them into the child process environment.
Branch builds receive zero release credentials, always.

Trusted infrastructure changes use a separate, phase-explicit contract. An
actor authorizes `plan` for one exact already-green SHA and the target's exact
current base. Plan and Apply use different operator-owned secret paths:
`oberth/upstream/<org>/<repo>/plan/<name>` and
`oberth/upstream/<org>/<repo>/apply/<name>`. A Plan can never fetch Apply
credentials. The resulting plan file is capped at 16 MiB, encrypted through
OpenBao Transit before it reaches Oberth's PVC, and bound to the exact source,
target base, pipeline/lockfile identity, actor, and expiry. Promotion publishes
Git first; only then does Oberth enqueue the bound artifact for one-shot Apply.
Promotion evidence and Apply status remain distinct because Apply failure does
not roll Git back or make publication unsuccessful.

Setup is one script run where your own `bao`/`vault` CLI is authenticated —
the Oberth binary has no code path that accepts a store admin token — and
`oberth secretstore verify` proves the whole trust chain from inside the pod.
On verified HTTPS the setup also creates one non-exportable Transit key for
trusted-plan envelopes; its policy grants only that key's exact encrypt and
decrypt endpoints. The installer-managed production path generates listener
TLS material directly on OpenBao's retained PVC, publishes only the public CA
through a ConfigMap, and never starts a plaintext listener.

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
  --address http://openbao.openbao.svc:8200 --namespace oberth --disable-transit

# 4 · Oberth itself
helm repo add oberth https://charts.cloudtaser.io/oberth
helm install oberth oberth/oberth -n oberth \
  --create-namespace --set secretstore.enabled=true \
  --set secretstore.address=http://openbao.openbao.svc:8200 \
  --set secretstore.insecureHTTPForDev=true \
  --set secretstore.transit.enabled=false

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
2. **Run** — Oberth submits the repository's Argo Workflow YAML as an Argo
   Workflow. Each template runs in its own container with its own exit code,
   timeout, and named log slice. The server sets metadata, forces the
   ServiceAccount identity, and injects the source checkout.
3. **Record** — green publishes the exact branch SHA to the upstream forge;
   red creates or updates the branch's single CI issue. Nothing lives only on
   the box.
4. **Promote** — `promote <sha> <branch>` verifies the SHA is green, merges
   locally, proves the merged tree when the target diverged, and pushes
   without force.
5. **Release** — a tag reachable from the default branch runs the release
   Workflow. The presence of declared secret-store paths in the
   `oberth.ci/secret-paths` annotation selects the credentialed
   ServiceAccount and its projected token. Release steps wrap their command
   with `envconsul`, which authenticates to OpenBao via Kubernetes auth and
   injects the declared secrets into the child process environment. Only
   after every burn is terminal green does Oberth sync the exact admitted
   tag object upstream.

## The pipeline is Argo Workflow YAML

Each repository carries its pipeline as standard Argo Workflow YAML in the
`.oberth/` directory: `.oberth/build.yaml` for branch-triggered CI,
`.oberth/release.yaml` for tag-triggered releases. Oberth submits the YAML as
an Argo Workflow, setting metadata, forcing the ServiceAccount identity by
trigger tier, and injecting the source checkout into every container template.
Tool installation is inline pipeline steps -- fetch, pin, unpack -- each with
its own named exit code. Version pins live in repository files next to the
code that uses them:

```yaml
# .oberth/build.yaml (branch-trigger, abbreviated)
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: L
spec:
  entrypoint: ci
  activeDeadlineSeconds: 3600
  templates:
  - name: ci
    dag:
      tasks:
      - name: setup
        template: setup
      - name: lint
        template: lint
        depends: setup
      - name: test
        template: test
        depends: lint
      - name: build-amd64
        template: build-amd64
        depends: test
      - name: build-arm64
        template: build-arm64
        depends: test

  - name: lint
    steps:
    - - name: vet
        template: vet
    - - name: golangci-lint-run
        template: golangci-lint-run

  - name: vet
    container:
      image: "golang:1.26-trixie@sha256:..."
      workingDir: /work/src
      command: ["/tmp/oberth-tools/bin/go"]
      args: ["vet", "./..."]
      # ... env and volumeMounts
```

Each template is a container with its own exit code, timeout, and named log
slice. Oberth reads the YAML statically at submission -- it never executes
repository code in the server process.

## MCP tools (18)

| Tool | Description |
|------|-------------|
| `status` | CI status for a SHA or branch, including the failed step |
| `logs` | One named step's log output for a SHA |
| `run_get` | One exact durable run and its named step results by run ID |
| `run_logs` | One exact burn/step log by durable run ID |
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
| `access_list` | Show all secret access grants for a repo |
| `access_allow` | Grant a repo+step access to a named secret |
| `access_revoke` | Revoke a secret access grant |

Authenticated `GET` endpoints (`/api/runs`, `/api/repos`, `/api/issues`,
`/api/status`) serve the same state; `/healthz` and `/readyz` are
unauthenticated.

## Architecture

- **One pod, two ports** — SSH on NodePort `30022`, HTTPS/MCP on `30443`
  (TLS 1.3 or newer). Argo Workflows provides the execution engine.
- **One Workflow per run** — each push submits an Argo Workflow from the
  repository's `.oberth/build.yaml` or `.oberth/release.yaml`. Each DAG task
  runs in its own container. Repository setup steps install pinned,
  checksummed tools into a shared workspace volume.
- **PVC for state** — SQLite, bare Git caches, run workspaces, and retained
  logs on one volume, kept across uninstall. hostPath Go caches are split by
  repository *and* trust tier, so a poisoned branch build can never feed bytes
  into a signed release artifact.
- **Audit chain, external anchoring opt-in** — the gap-free SHA-256 action
  chain always runs and is verified at startup, on every cycle, and before
  every mutation. By default (`auditAnchor.tsaURL` and `auditAnchor.rekorURL`
  empty) no external service is contacted — Rekor and public TSAs are free
  services with no SLA, and a kind-on-a-laptop install should never depend on
  them. Opting in layers on: RFC 3161 timestamps from an independent TSA →
  linked Rekor witnesses under a stable identity derived from the SSH host
  key → immutable Kubernetes continuity records the server can create but
  never modify. With both configured, a whole-database rollback fails closed
  instead of forgetting.
- **Degraded-mode startup** — with the Rekor witness enabled but unreachable
  at startup, the pod comes up running-but-not-ready with every mutation gate
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
- **Declared paths and the trigger together are the security boundary.**
  Pipelines without declared secret-store paths bind to the pipeline
  ServiceAccount and receive no secrets on either trigger. Release pipelines
  with approved paths bind to the credentialed ServiceAccount — the only
  identity whose OpenBao role carries release-secret grants; branch (CI)
  pipelines with approved paths bind to the separate ci-secrets
  ServiceAccount, whose role reaches the upstream subtree only, so release
  credentials are unreachable from a branch push at the Vault layer. Branch
  pipelines may only declare upstream-scoped paths; system-namespace paths
  require a release trigger and an administrator allowlist entry. Explicit trusted Plan and Apply
  triggers receive only their distinct, phase-scoped paths; neither
  phase can consume the other's operator-owned path namespace. All fetched
  secret-store values are delivered to memory only.
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
[`.oberth/build.yaml`](.oberth/build.yaml) and
[`.oberth/release.yaml`](.oberth/release.yaml), and an admitted tag runs the
release workflow that builds, signs, publishes, and reads back every artifact.
Those workflows hold no upstream-forge mutation credential; Oberth publishes
the exact annotated tag only after the run is green.

## Web UI
<img width="1456" height="773" alt="image" src="https://github.com/user-attachments/assets/33684b0e-f2ef-4f77-8754-f17a593179e5" />


## Documentation

- [docs/mcp-setup.md](docs/mcp-setup.md) — connect Claude Code or any MCP client
- [docs/architecture.md](docs/architecture.md) — run lifecycle and recovery
- [docs/security.md](docs/security.md) — security invariants
- [docs/secretstore-verification.md](docs/secretstore-verification.md) — three-tier secret-store verification
- [docs/helm-service-migration.md](docs/helm-service-migration.md) — two-stage ownership-safe Service migration
- [docs/upgrade-schema-v3.md](docs/upgrade-schema-v3.md) — upgrading across a backup-and-replace store schema boundary (v2 → v3)
- [AGENT-CONTRACT.md](AGENT-CONTRACT.md) — the cross-component compatibility contract

## License

Apache License 2.0 — see [LICENSE](LICENSE).
