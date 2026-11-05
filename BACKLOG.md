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

- **Privileged eBPF test tier** (from #707): branch Workflow Pods are unprivileged by
  contract (no `privileged`, no SA token). Kernel-loading eBPF tests need an
  operator-owned privileged tier (KubeVirt VM or equivalent) with the exact
  candidate SHA bound into the run and pin cleanup proven. Until designed,
  privileged eBPF validation stays outside Oberth CI.
- **Trusted cross-repository checkouts** (from #709): a provider-owned,
  allowlisted read-only checkout of a second repository inside a Workflow Pod (e.g.
  CLI validating the Helm chart schema byte-for-byte). No primitive exists in
  the Workflow model today.
- **Resource measurement completion** (from #754): per-template history and
  read-only suggestions are implemented. Complete representative measurements
  and commit reviewed Pod resources in repository YAML. Kubernetes places the
  Pods; Oberth never rewrites resources. See [measured requests](docs/measured-requests.md).
- **Adaptive GOMAXPROCS** (from #755): auto-derive GOMAXPROCS from the Workflow Pod's
  cgroup quota. The admission load guard half of the original entry shipped
  in v0.10.57 as size-weighted scheduler admission.
- **Branch Workflow egress policy option** (from #691 residual): the inline
  pinned-tool pattern requires egress, and branch Workflow Pods hold no secrets — but
  an optional chart NetworkPolicy restricting branch-pipeline egress to declared
  tool endpoints would add defense-in-depth against exfiltration of source
  not yet public and cache-poisoning callbacks. Also from #691: per-step
  clean `HOME`/`GOENV` isolation inside a burn.
- **Release resume-from-boundary** (from #818 residual): a release Workflow is
  retried only through the recorded run/replay rules; an infrastructure failure after
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
  Artifacts provide the transport; protected periodic retention and audited
  per-run purge are implemented by #1101.

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
- **Issue queue dispatch layer** (from #798 remainder): `issue_lock` (claim
  lease), global `issue_list`, and green-auto-close shipped in the rewrite.
  Remaining: a blocking `issue_watch` (from #800), priority labels, and a
  dashboard issues panel (from #803).
- **Per-identity CI scoreboard** (from #790): aggregate pass/fail by uplink
  identity over the runs store; `GET /api/identities` + dashboard panel.
- **Ingress discovery endpoint** (from #725/#809 residual): Service type and
  client address are configured at installation; a read-only endpoint serving the SSH host public key and TLS fingerprint
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
historical release issues; the current contracts live in [the architecture](docs/architecture.md). Landed in v0.10.55:
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
  release operator, not yet by the release template. Wire the template to template
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
  download-and-verify procedure in the README, and `latest/VERSION`
  monotonic convergence observed across two sequential tags.
- **OpenBao HA qualification** (OBERTH-RELEASE-005 residual): qualify raft HA,
  StorageClass behavior, unseal-share custody, root-token rotation, snapshots
  and audit devices. The installer supports persistent verified-TLS OpenBao
  through `--install-secretstore`; generic `--dev` and `--production` mode
  flags have been removed. The separate `--install-secretstore-dev` option
  selects the documented in-memory evaluation store.
- **Multi-arch OCI images** (OBERTH-RELEASE-011, landed in v0.10.56): the
  Release templates publish the server image as a two-child OCI index
  (linux/amd64 + linux/arm64) repacked from a multi-arch substrate index,
  and the chart pins the index digest. The former runner image and its
  evidence tooling are retired: Argo Workflow templates use repository-declared tool images and commands
  with server-injected source and an Oberth credential wrapper.
- **Node Budget — continued qualification** (OBERTH-RELEASE-012): declared
  admission classes, per-template Pod requests, resource measurement and
  numeric recommendations are implemented. Continue qualifying capacity
  accounting under contention and incomplete observations; see
  [declared admission](docs/declared-admission.md) and
  [runtime capacity](docs/runtime-admission-capacity.md).
