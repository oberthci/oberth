# C13 — Own pipelines

## Purpose

C13 supplies Oberth's executable reference pipelines and their inspection harnesses.
They prove that admitted source becomes tested, signed and verified release bytes
through ordinary native Workflows. They also exercise installation, source delivery,
logs, UI and upgrade contracts. C13 does not schedule runs, implement a dependency
engine, grant authority, define server admission, retain run artifacts or deploy a
particular installation. It adds no CLI, MCP, API or event service. Native execution
belongs to C5; publication of Git refs belongs to C2/C6; installation belongs to C9.

## Owned data and public contract

C13 owns the release-pipeline executable `cmd/oberth-release` and
`internal/releaseverify`. They verify/build/publish release
bytes through explicit credentials and bounded public clients; they are leaf tools, not
server composition roots. Shared `internal/staticgit` provides their pure packaging
records. C7 owns test-only `internal/testdb` and `internal/storetest`.
Production imports no test helpers.

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

- **Documents.** `build.yaml` and `release.yaml` are single native Workflow documents
  with `apiVersion`, `kind` and `spec`; ownership metadata and execution identities come
  from admission. Branches, promotions and ordinary tags select the build document by default.
  Version tags and configured release routes select the release document under C2/C4
  rules. The build document alone supplies branch and promotion evidence; the
  product has no promotion-file selection. The build declares no resource
  template, so admitting it needs no capability; the VM install walk is the
  separate `hack/walk-vm.yaml`, selected only by explicit push option. There is no new route or
  special run class. `.oberth/issues.yaml` retains the
  opt-in `{close_from_trailers: true}` policy; C2 owns its interpretation.
- **Visible work.** Build has three leaves: `make test`, `make static` and
  `make release-bytes`; promotions run the same leaves. Release has build, sign,
  publish, verify and finalize leaves. Preserve dependency gates and distinct logs/status. The C4 admission plan
  supplies their identity and count; harnesses and UI must not infer steps from shell
  commands or count structural nodes as work. **Delta:** consolidate repeated source,
  environment and resource declarations with native defaults and parameterized
  templates. Remove only unused mappings `OBERTH_REPO`, `OBERTH_REF`, `OBERTH_SHA`,
  `OBERTH_TRIGGER`, `OBERTH_RUN_ID` and `OBERTH_STEP`; preserve consumed environment and
  commands. Existing namespaced `templateRef` may share ungranted work only after its
  template is installed and admitted. Granted leaves remain inline under their exact
  workflow-file/step grant. Do not add a fragment loader, YAML generator language or
  private dependency expansion.
- **Source and storage.** Every known source consumer declares its own input: `name:
  source`, `path: /work/src`, `mode: 0755`, `plugin: {name: oberth, key:
  '{{workflow.parameters.oberth-source}}', configuration:
  '{"version":1,"kind":"main"}'}`. The source plugin is read-only; each Pod gets a
  separate copy. Native claims, mounts, subpaths, cache environment and scratch are
  authored explicitly. require explicit source and cache declarations without injection.
  A `work` name has no implicit behavior. Persistent cross-run caches require C2/C7's
  repository/tier PVC grants; a per-Workflow claim is not a cross-run cache.
- **Facts and credentials.** Authors map `oberth-repo`, `oberth-ref`, `oberth-sha`,
  `oberth-trigger`, release facts and `workflow.name` into their own environment. No
  injected run-fact environment is assumed. Granted leaves declare their client,
  exact-audience read-only login projection, public CA and memory scratch. Authority
  remains the exact canonical repository/workflow-file/step grant, never an annotation
  or command. The signing leaf alone receives the signing key; publish and
  finalize alone receive the GitHub release token, each through its own grant. Installation-specific
  audiences come from installation-owned configuration through the existing
  declaration and capability contracts. Accounts follow C5's
  [release authority](c5-native-execution.md#release-authority).
  No endpoint, provider account or operator identity is embedded in the
  reference example; credentials never become parameters.
- **Delta:** **Release outputs.** Four platform executables, digest-bound server and source-
  plugin images, the packaged chart, signed `SHA256SUMS` and `release.json` carry
  every shipped digest. Install verifies them before any write. No registry
  signatures or chart index. The clean-Pod verify leaf checks signed bytes and
  digests before finalizing `latest`. C3's gate delivers the admitted tag's exact
  object upstream before the run exists; the release never pushes that tag itself.
- **Harness evidence.** Repository-local executable checks under `hack/` fail on
  nonzero exit. Candidate walk mode installs this run's bytes through C9's
  local-chart path; release mode verifies a published signed release. Retain
  source/image identities, command/environment, deadlines, limits, hashes and
  measured wall/RSS/storage peaks. Configured bounds are not measurements.

### First-release limits

The first release is made by hand with `hack/release.sh` and the documented
release steps. Admitted release facts follow C3's
[server-only hook](c3-wiring-and-submission.md#owned-data-and-public-contract)
and C4's [admission contract](c4-admission-rules-and-plan.md#owned-data-and-public-contract).
Account selection follows C5's [release authority](c5-native-execution.md#release-authority);
there is no configured role-mapping prerequisite, shared-account workaround or
promotion-file binding.

### Release trust

C13 owns the canonical release public key. `hack/release-pin.sh <public-key-file>`
provisions it and C12's generated fetch key material in one operation. C12 owns
[fetch verification and the agreement check](c12-agent-client.md#owned-data-and-public-contract).
Public release consumers and publication use C11's canonical release origin: each tag's GitHub release.

Release signatures are checked only against the pinned public key in the release
script, product verifier and fetch script, with no transparency log. Signing
needs no network access.

### Check tooling

**Delta:** Once pinned dependencies and build-image tools are provisioned,
`make check` requires no external network: no tool installation, metadata lookup or probe
may be hidden in a check. Pin the linter and reachability tool in their tool
modules, retain the tool's own dependencies and use the ordinary shared caches.
A missing required tool or dependency refuses plainly; it never becomes a skip
or selects an unpinned substitute. Every Go test has an explicit timeout.

Private names and matched pattern text never appear in guard diagnostics,
including a matching filename, link target or malformed policy. Report a masked
location and pattern index instead. Host product-name exemptions are limited to
plugin folders, manifests and the installation guide's Agent plugins section;
authorship credit has no exemption.

## Allowed dependencies

C13’s exact allowlist and compiler-fixture exceptions are in
[dependencies.json](dependencies.json). Ordinary harness Go imports only narrow
`internal/staticgit` packaging values; pipelines and scripts call public commands.
Disposable fixture-only imports are restricted to `hack/upgrade-e2e/fixture/**` and
named compiler overlays under `hack/testdata/**`; none reaches production builds. Those
fixtures may exercise C3–C9 as explicitly listed, never a second admission/execution
path.

## Invariants and budgets

Installation policy follows [C9 invariant 7](c9-install-and-upgrade.md#invariants).

1. Use the same native decode/admission/plan contract as submission. List all offending
   declarations with actionable locations. Missing references, grants, identities, pins
   or source never silently fall back. The plan is static evidence; it does not prove an
   image starts or a cold build fits. Source-consuming templates expose values through
   output parameters; no task references their `outputs.result`.
2. Build CI has no release credentials. Ambient token automount stays false. Secret
   consumers explicitly declare projections and private memory scratch; tests and
   downloaded-artifact execution receive no publisher credentials or trusted
   publisher-tool write access. Verify admitted source and executable pins before
   credential use. Installation review and policy choices belong to C9.
3. **Delta:** Tests run once, uncached, with the race detector over `./...`; tools are
   pinned in `go.mod`. A failing test, missing event or cleanup failure cannot
   become green through wrapper success. No vulnerability scanners run in the
   own pipelines. Assist remains off pending review and real-cluster checks;
   runs with an audited inspection exec or an armed step without a succeeded final
   native result lose evidence eligibility. Asking for assist alone does not. Tests on the gittest fixture run the packaged static Git, exported from the run's checked base and named in `OBERTH_TEST_GIT`, so they wait for the base pull: a failed pull leaves the push without a test result, and resource requests govern concurrent Go steps.
4. Resource requests, limits, scratch, deadlines, parallelism and synchronization remain
   YAML-owned. Keep native workflow/publication locks where already authored. No server
   queue, CPU math, capacity gate or injected release lock. Configured build and release
   deadlines are 3600 seconds. Heavy Go steps request their full memory limit;
   capacity uses requests and affinity. Publication uses a repository-scoped mutex.
   Oberth holds no lock sizes. The waiters-wake walk proves native wake-up after cancellation/failure.
5. **Delta:** Release runs no tests. It builds the tagged commit at the default branch's
   head, which a passing build run tested. The release step checks those facts
   and refuses any mismatch. No branch, promotion or release run starts a VM;
   only the walk's separate document declares one. VM and static-Git base images are locally built
   and pinned by digest.
6. Charge source and delivered metadata, writable root, private caches, compiler scratch
   and logs to the appropriate bounded storage. Charge memory-backed volumes to memory.
   Report per-Workflow PVC consumption separately, including build tools/caches; enforce
   its authored size and read-only aliases. Never exempt source through a host bind or
   write shared developer caches. A live packaged-plugin source-consuming run must prove
   actual delivery and storage.
7. Launchers bound child work, preserve original failure/signal identity and join/clean
   their owned processes and scratch. Step deadlines use native `activeDeadlineSeconds`. Disposable live checks use explicit private cluster
   configuration; never use an ambient installation context. Local heavy-run
   coordination belongs to the harness, not server scheduling.

Automated release publication, enabled after cutover, uses only an admitted
release run. The GitHub release token reaches only the publish and finalize steps and
the signing key only its granted signing step.
One build target produces
the matching server image, source-plugin image and chart, each with a declared command,
for local use and release. `ux-gate` records complete command, flag and time baselines
through the first passing build and release. From fresh install, at most 8 commands
and 10 flags reach the first green build; at most 4 more commands reach the first
release. The reference path takes 18 commands to first green. Missing observations
fail the gate. CI enforces command and flag counts and records times; the
25% time-growth check runs in the measured local walk.

## Inspections

The requirement sentence is the inspection contract. These inspections are planned.

| Requirement | Inspection |
| --- | --- |
| [REQ-C13-001](REQUIREMENTS.md#req-c13-001) | `TestExactShortLivedPublicationAuthorityContract` (contract_test) |
| [REQ-C13-003](REQUIREMENTS.md#req-c13-003) | `TestReproduciblePackagedBytesContract` (contract_test) |
| [REQ-C13-013](REQUIREMENTS.md#req-c13-013) | `TestMonotonicReleaseFinalizationContract` (contract_test) |
| [REQ-C13-016](REQUIREMENTS.md#req-c13-016) | `push` (walk_step) |
| [REQ-C13-017](REQUIREMENTS.md#req-c13-017) | `ux-gate` (walk_step) |
| [REQ-C13-018](REQUIREMENTS.md#req-c13-018) | `ux-gate` (walk_step) |
| [REQ-C13-019](REQUIREMENTS.md#req-c13-019) | `ux-gate` (walk_step) |
| [REQ-C13-020](REQUIREMENTS.md#req-c13-020) | `push` (walk_step) |
| [REQ-C13-021](REQUIREMENTS.md#req-c13-021) | `release` (walk_step) |

The tooling inspection runs the complete check with external networking disabled after
provisioning the pinned inputs, and supplies private-name fixtures in content,
paths and malformed policy to prove diagnostics disclose only masked locations
and pattern indexes. The push walk admits the same build document for branch
and promotion work, with no capability granted; no alternate promotion file
or narrower template inspection earns a pass.

`push` exercises the paired local build through real source delivery with
explicit image commands. `ux-gate` records fresh and warm command/flag/prompt/time
baselines, enforces command and flag ceilings in CI and checks the 25% time bound
locally. `TestExactShortLivedPublicationAuthorityContract` changes repository,
file and step grants and refuses release grants in build CI.
`TestReleaseCredentialVolumes` checks that publish/finalize declare only the GitHub token
path and that the verify step has no publication token.
The clean-Pod verify step checks signed bytes, component inventories and digests
before moving `latest`. Finalization tests cover create-only version objects,
monotonic latest marking and refusal of different bytes on rerun.

## Retired terms

**Delta:** Injected source/cache/run facts → authored native YAML; fragment expansion →
native templates; publishing credentials → short-lived delegation; signing key → exact signing grant.

The image-push leaf alone exchanges its exact-audience projected identity through
cloud federation for a short-lived registry login in private memory scratch. A
stored JSON key is only an alternative for clouds without federation, never an
automatic fallback.
