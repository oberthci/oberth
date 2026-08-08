# Fresh rewrite review gates

The rewrite is reviewed between implementation stages so an old component or
assumption cannot silently re-enter while a useful behavior is being rebuilt.
Each gate compares the complete staged tree with the preserved `origin/main`
history and treats the binding FAB architecture documents as the only product
source.

## Stage 1: primitive boundary review

Completed 2026-08-04 before control-plane composition.

Reviewed surfaces:

- domain model and SQLite migrations;
- Git clone-through cache and workspace creation;
- authenticated SSH smart-protocol boundary;
- Job rendering and Job-side pipeline runner;
- log redaction, retention, and indexing;
- HTTPS/MCP transport primitives;
- namespace-scoped Helm resources.

The gate used independent correctness, standards, testing, maintainability,
agent-accessibility, security, performance, API-contract, migration, reliability,
and clean-room provenance reviews, plus an independently receipted adversarial
model pass and a separate validator.

Provenance result:

- no old implementation package, controller, reconciliation loop, shadow
  default branch, workflow object, release controller, or dashboard subsystem was
  copied into the fresh tree;
- no obsolete product name appears in the fresh tree;
- the old `.oberth/ci.yaml` is absent; the replacement `.oberth/periapsis.go`
  contains only the fresh lint, race-test, scan, chart, and binary-build gates;
- retained behavior is limited to the FAB-specified Git staging and Job-side
  pipeline execution semantics, implemented against new package boundaries.

The reviewers produced 39 raw primary candidates. Semantic reconciliation and
the binding-spec check reduced these to 27. Independent validation rejected
three unsupported findings and retained 24 fixes. The retained work is grouped
around Git/SSH acceptance, identity and fresh service composition, Job/runner
lifecycle, bounded log storage, and accountable SQLite issue state.

Stage 1 closed after those fixes passed focused race tests and the complete
mandatory local validation suite.

## Stage 2: control-plane architecture review

Completed 2026-08-05 after the new server, scheduler, MCP tool service, CLI
administration, image contracts, and Helm arguments were composed. The gate
proved:

- every external action resolves to a durable uplink identity;
- accepted pushes are replayable and create exactly one run;
- newer-push interruptions retain supersession metadata and durable Job
  cancellation obligations;
- branch and tag trust domains remain separate at the Secret-mount boundary;
- promotion uses the tested tree and a non-forced target push;
- all waits, logs, queues, and ingress paths are bounded;
- no old component or obsolete naming was introduced during composition.

The first pass found publication races, incomplete actor binding, unbounded
reads, and a mutable runner-image trust path. The repaired tree passed focused
race tests and the mandatory `vet`, race, and lint suite. A separate durability
rereview found no remaining P0/P1 control-plane defect.

### Runner supply-chain subgate

The official Kubernetes 1.35.7 envtest binaries failed the image policy with 22
High and one Critical finding. The replacement bundle is rebuilt from the exact
official source archive with patched dependency pins and Go 1.26.5; two isolated
builds produced byte-identical `kube-apiserver` and `kubectl` hashes, the pinned
database scan found zero High/Critical findings, and the exact bundle passed the
operator startup and network-policy race tests.

The final image rebuild also rejected the official Helm 4.2.3 binary because it
embeds `oras-go` 2.6.1's unsafe crafted-tar hardlink handling. Helm is now built
from its immutable 4.2.3 module source with only `oras-go` raised to the 2.6.2
repair, an Oberth derivative identity, and a byte-reproducible pinned digest.
The evidence exporter copies only the required scanner binary instead of
exporting the complete Go build stage.

An across-run comparison then exposed a timestamp written into
`/var/log/apk.log`: the gate's second build had reused layers and therefore
masked the drift. Both runtime Dockerfiles now remove that log, and both runner
reproducibility builds bypass the layer cache and use distinct Go module and
compiler cache namespaces before their manifests are compared.

Publication now uses an exact checksum-pinned `crane`, bounded commands,
exclusive attempt state, complete OCI graph read-back, and a deployment gate
that requires the latest passing publication plus fresh, complete tag and digest
OCI graph pulls. The gate uses a private rehashed client copy, an explicit empty
credential file, and no ambient Podman credential sources. The full clean runner
OCI build/scan and registry publication remain part of the Stage 3 gate; no
image is accepted merely from these component-level proofs.

The final publication-security pass found and repaired four boundary gaps: an
installed client was executed after checking a mutable pathname, shared locks
lived directly in a world-writable temporary directory, the OCI fixtures did
not distinguish missing from unreachable blobs or verify the exact pull refs,
and `promote` returned an internal record instead of only its durable ID. The
current-hash rereview found no actionable P0/P1/P2 issue after those repairs.
Shell syntax, ShellCheck, the focused race suites, and the complete mandatory
local validation suite are green.

The final joined-tree rereviews also found and closed receive-boundary gaps:
public ref names now use one grammar at SSH admission and upstream
materialization, legacy invalid cache refs are removed before reservation, the
exact release-admission SHA survives callback replay and upstream default-branch
renames, full symbolic `HEAD` handles a same-name branch/tag without ambiguity,
and Job request preparation/Create cannot be overtaken by superseding deletion.
Three independent post-fix reviews reported no actionable P0/P1/P2 findings.

### Exact FAB scope conformance

The Stage 2 tree maps completely to the binding FAB design:

- Helm, admin, auth, and store code implement the pod, fixed NodePorts, PVC,
  persistent TLS/SSH identity, upstream bootstrap, and uplink token model.
- SSH, Git cache, receive outbox, push admission, and publication code implement
  clone-through caching, stale service, periodic GC, accepted force-pushed
  branch refs, durable actor attribution, green sync, and non-forced promotion.
- Scheduler, Job, runner, Periapsis, log, and redaction code implement one
  single-container Job per run, FIFO concurrency, supersession, isolated cache
  and Secret domains, sequential subprocess steps, bounded named logs, and the
  exact four-value termination-step status vocabulary.
- API, service, issue, and projection code implement only the FAB web views and
  exact MCP surface: status, logs, wait, sync, promote, promote status, and the
  seven issue actions.
- Dockerfiles and `hack/runner-*` files are deployment evidence and immutable
  build-environment safeguards; they do not add control-plane behavior.

The conformance tests reject an extra MCP tool, an extra termination-step
status, the removed static CI definition, the obsolete product name, and legacy
workflow-controller markers. No Argo support, multi-container step controller,
separate release controller, shadow branch, cluster-wide RBAC, or additional
dashboard subsystem is present.

A staged-tree blob identity audit against the preserved pre-rewrite `main`
found exactly one unchanged file: `LICENSE`. Aggressive 20-percent Git
similarity detection produced three candidates, each reviewed line by line:
the repository's Periapsis file and two Helm templates. Their overlap is limited
to required Go/Helm syntax and FAB-prescribed fields; none retains an old
controller, release, reconciliation, or workflow implementation.

## Stage 3: deployment and real-flow review

Run before the first real release. It covers rendered manifests, immutable image
digests, restored Secret names, cache ownership, namespace RBAC, TLS/SSH identity,
and the complete feature-red, issue-update, supersede, green-sync, divergent,
fast-forward, and already-contained promotion paths, reachable-tag release,
unreachable-tag rejection, and post-release tag immutability trace.

The old k3s installation is removed only after the replacement commit is safely
published and its pre-deployment validation is green.
