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
- **Per-run resource classes** (from #754, reframed for the one-Job model):
  all runs share one resource request/limit shape from the chart. A
  repository-declared light/standard/heavy class mapped to bounded Job
  resources would let lint-only runs coexist with heavy builds on small
  nodes.
- **Adaptive GOMAXPROCS + admission load guard** (from #755): auto-derive
  GOMAXPROCS from the Job's cgroup quota; optionally refuse to admit new runs
  when node load exceeds a threshold.
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
  releases.cloudtaser.io with a download cache).

## Periapsis v2 (epic #684 remainder)

- **OnFail evidence blocks** (from #686): collect declared files/logs on step
  failure, redacted, retained with policy.
- **Topology multi-container smoke tests** (from #687): N containers in one
  pod with name-based addressing and health gates; 30-second integration
  smoke for port/env/cert/fingerprint failure classes.
- **Assert typed test blocks** (from #688): typed HTTP/gRPC/exec assertions
  with structured expected-vs-actual output, scoped to the topology pod.
- **Artifact storage** (from #690): checksummed, access-controlled store for
  build outputs and failure evidence, addressable by SHA and step.

## MCP and client surface

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

## Fleet (component repositories)

- **cloudtaser-helm periapsis adoption** (from #658 residual): the chart repo
  still has no `.oberth/periapsis.go`; lint/template/kubeconform/version
  coherence per the #658 spec.
- **Cross-compile env fixes in cli/ebpf/port/wrapper** (from #797): their
  build burns never set `GOOS`/`GOARCH`; `Go.BuildFor` now exists and
  pipeline admission rejects byte-identical build steps, so each repo needs
  its one-line periapsis.go fix **before** upgrading its Oberth to a version
  carrying the new validator. Filed per-repo on Codeberg.
