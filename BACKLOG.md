# Oberth backlog

Deferred work, tracked here so the issue tracker reflects only actionable
current-release work. Every entry names the Codeberg issue it came from;
reopen or refile that issue when an entry's implementation begins. Entries
marked **deadline-bound** carry an external clock.

## Audit witness chain

- **Witness search redesign + Rekor v2 migration** (from #828, **deadline-bound**):
  `AllowMutation` still performs a Rekor `SearchExact` (POST `index/retrieve`,
  a documented best-effort API) whenever the local audit head is ahead of the
  last completed witness — a search-API degradation fails all mutations closed
  for its duration (integrity preserved, availability lost). Redesign
  direction from #828: scope the exact-search to unresolved-intent
  crash-window reconciliation and prefer completing a fresh anchor for
  ordinary post-anchor head advance. Needs a dedicated security review.
  Rekor v2 (rekor-tiles) removes the search index API entirely, so the
  current lookup has a hard replacement deadline before v2 migration.
- **Opt-in verbose server logging** (from #835 item 6): single-line startup,
  cache-hit vs upstream-fetch visibility on clone; today all logs are
  unconditional and quiet.

## CI platform

- **Privileged eBPF test tier** (from #707): branch Jobs are unprivileged by
  contract (no `privileged`, no SA token). Kernel-loading eBPF tests need an
  operator-owned privileged tier (KubeVirt VM or equivalent) with the exact
  candidate SHA bound into the run and pin cleanup proven. Until designed,
  privileged eBPF validation stays outside Oberth CI.
- **Trusted cross-repository checkouts** (from #709): a provider-owned,
  allowlisted read-only checkout of a second repository inside a Job (e.g.
  CLI validating the Helm chart schema byte-for-byte). No primitive exists in
  the one-Job model today.
- **Per-run resource classes — Job-shape residual** (from #754): v0.10.57
  shipped repository-declared step sizes (`WithSize` S/M/L/XL), the
  server-side `DeclaredMaxSize` static analysis, weighted node-budget
  admission, and durable per-step `declared_size` evidence. Remaining from
  the original entry: mapping the declared class onto distinct bounded Job
  resource request/limit shapes (all Jobs still share one shape from the
  chart).
- **Adaptive GOMAXPROCS** (from #755): auto-derive GOMAXPROCS from the Job's
  cgroup quota. The admission load guard half of the original entry shipped
  in v0.10.57 as size-weighted scheduler admission.
- **Branch-Job egress policy option** (from #691 residual): the inline
  pinned-tool pattern requires egress, and branch Jobs hold no secrets — but
  an optional chart NetworkPolicy restricting branch-Job egress to declared
  tool endpoints would add defense-in-depth against exfiltration of source
  not yet public and cache-poisoning callbacks. Also from #691: per-step
  clean `HOME`/`GOENV` isolation inside a burn.
- **Release resume-from-boundary** (from #818 residual): a release Job is
  atomic today (`BackoffLimit: 0`); an infrastructure failure after
  publication requires re-running the whole release identity. Exact-boundary
  resume with artifact revalidation remains future work; the original
  get.helm.sh root cause is closed (all tools checksum-pinned from
  releases.oberth.ci with a download cache).

## Pipeline features (post-Argo migration)

The periapsis.go Go-DSL pipeline path is retired in favor of Argo Workflow
YAML. The following items from the original Periapsis v2 epic (#684) are
reframed for the Argo execution engine where they still apply:

- **OnFail evidence blocks** (from #686): collect declared files/logs on step
  failure, redacted, retained with policy. In Argo, this maps to
  `onExit` handlers or artifact collection templates.
- **Topology multi-container smoke tests** (from #687): Argo natively supports
  sidecar containers and DAG-based multi-service topologies, making this
  easier to implement than under the single-Job model.
- **Artifact storage** (from #690): checksummed, access-controlled store for
  build outputs and failure evidence, addressable by SHA and step. Argo
  Artifacts provide the transport; Oberth-side retention policy is the
  remaining work.

## MCP and client surface

- **Admin list/remove API+MCP equivalents** (OBERTH-RELEASE-018 residual):
  `upstream list|remove`, `repo add`, and `uplink list|remove` shipped as
  audited in-pod CLI commands in v0.10.56 (uplink removal revokes the bearer
  token in the same transaction; upstream removal fails closed on immutable
  CI history). Remaining: read-only API/MCP equivalents, and on-disk gitcache
  directory cleanup for removed mappings (inert without a mapping; a re-added
  repository refreshes its cache from upstream).
- **`oberth watch` CLI** (from #789): follow a push's CI outcome from the
  terminal on top of the server's `wait` long-poll; exit non-zero on red.
- **`repos_list` MCP tool** (from #822): the 13-tool surface is deliberately
  minimal and `GET /api/repos` serves the inventory today; add the MCP tool
  only with a deliberate contract revision if agent workflows keep needing
  it in-band.
- **Issue queue dispatch layer** (from #798 remainder): `issue_lock` (claim
  lease), global `issue_list`, and green-auto-close shipped in the rewrite.
  Remaining: a blocking `issue_watch` (from #800), priority labels, and a
  dashboard issues panel (from #803).
- **Per-identity CI scoreboard** (from #790): aggregate pass/fail by uplink
  identity over the runs store; `GET /api/identities` + dashboard panel.
- **Ingress discovery endpoint** (from #725/#809 residual): NodePorts are
  fixed and knowable, which retired the old resolve-before-push dance; a
  read-only endpoint serving the SSH host public key and TLS fingerprint
  would still simplify strict-host-key bootstrap for new workstations.

## Promotion and publication

- **Target-branch allowlist knob** (from #791 residual): `promote` accepts
  any explicitly named target branch (authenticated, green-gated, CAS,
  no-force). An operator-owned allowlist (`upstream.sync.publishableBranches`
  style) would narrow the surface for multi-tenant installs.
- **Publication/outbox aging signals** (from #793, reframed): expose
  oldest-pending age for accepted-push publication and failed-delivery
  retries in `/api/status`, with warn thresholds — burn-down needs
  instruments before pressure.

## Distribution and adoption (OBERTH-RELEASE wave, updated 2026-08-09)

Residuals from the OBERTH-RELEASE wave; full specifications live in the
workspace issue files `issues/OBERTH-RELEASE-*.md`. Landed in v0.10.55:
`oberth init` (004), `oberth install` dev-evaluation core (005), the
`oberthci/homebrew-tap` distribution point (002/013), and the clean-break
Test/Build/Done rename (015). Landed in v0.10.56: multi-arch OCI release
indexes (011), store-sourced release credentials via SecretStoreSecrets
(003 core), admin list/remove commands (018), and `repo add` — explicit
repository-to-upstream mapping, closing the gap where a multi-upstream
deployment could never admit a new repository (push-time discovery is
single-upstream-only by design).

- **Release-burn tap automation** (OBERTH-RELEASE-002/013 residual): the tap
  repo exists and serves the release formula, but updates are pushed by the
  release operator, not yet by the Release burn. Wire the burn to template
  the four checksums from the just-published SHA256SUMS, verify with
  `homebrew/verify-formula.sh` semantics, and push to the tap via a scoped
  credential declared in `ReleaseSecrets`. A release is then incomplete
  while the tap lags.
- **R2 binary publication contract** (OBERTH-RELEASE-003 residual): proven
  on v0.10.56 — the first real tag Release run through the deployed Oberth
  with store-sourced credentials: all nine public objects 200, fresh
  `sha256sum -c` + `cosign verify-blob` pass, every artifact's GOOS/GOARCH
  proven via `go version -m` (darwin-arm64 genuinely arm64), and
  `latest/VERSION` advanced to v0.10.56. Remaining: a macOS `oberth
  version` smoke on real hardware, docs standardization on the
  `.sigstore.json` bundle (no bare `.sig` anywhere), the public
  download-and-verify procedure in README/website, and `latest/VERSION`
  monotonic convergence observed across two sequential tags.
- **`oberth install --production`** (OBERTH-RELEASE-005 residual): the
  hardened path — OpenBao raft HA with persistent volumes, StorageClass
  preflight, and the completion security
  recommendations block (unseal shares, root-token rotation, raft
  snapshots, audit device, exposure review). The flag exists and
  hard-rejects until this lands. Verified installer-managed listener TLS now
  stages private material directly on the retained PVC before OpenBao starts,
  publishes only its public CA, and makes trusted-plan Transit structurally
  impossible over HTTP. Also from 005: a kind-based e2e job
  running `install --dev` + smoke `upstream add` in this repo's own
  pipeline.
- **Multi-arch OCI images** (OBERTH-RELEASE-011, landed in v0.10.56): the
  Release burns publish the server image as a two-child OCI index
  (linux/amd64 + linux/arm64) repacked from a multi-arch substrate index,
  and the chart pins the index digest. The former runner image and its
  evidence tooling are retired: Jobs run repository-declared standard
  golang images executing `go -C .oberth run .`.
- **Node Budget — step resource sizes** (OBERTH-RELEASE-012, epic-sized):
  `WithSize(oberth.S/M/L/XL)` on steps; weighted admission (never
  co-schedule two L+ across concurrent Jobs, XL exclusive, interactive
  reserve), size-mapped Job resources, per-step peak-RSS/CPU capture from
  wait4 rusage, measured-vs-declared divergence reporting, and
  `oberth suggest-sizes`. Full design:
  `00_Architecture/oberth/ci-robustness-design.md`. Phase 1 alone is 5–7
  days; never preempt-kill or cgroup-freeze running steps, no parallel
  steps within a burn, no auto-retry.
