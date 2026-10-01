# Watch CSA recovery — source implementation, unqualified

The narrow v2 implementation accompanies the chart label correction and
prepared regressions. No compilation, test, fixture or live recovery has run.
All execution remains held pending a fresh coordinator admission. Existing v1
keeps its deployed-revision guard. The implementation supports only the
original failed72 recovery; the design's initial-mode extension is deferred.

## Observed trigger and scope

The first supported v0.16.25 adoption confirmed four metadata CAS writes,
then normal Helm revision 72 failed. The immutable public classifier
`HELM72-FAILURE-CLASSIFICATION-v2.json`, SHA-256
`86d5f3d8f3ce67e787f27d7a5649d0c7c0252e09aa66524d1a1275111f8d2151`,
records six SSA conflicts, all with `kubectl-client-side-apply`:

| Object | Conflicting field |
| --- | --- |
| ServiceAccount/cloudflared-watch | metadata label app.kubernetes.io/name |
| ConfigMap/cloudflared-watch-openbao-ca | metadata label app.kubernetes.io/name |
| ConfigMap/cloudflared-watch-oberth-origin-ca | metadata label app.kubernetes.io/name |
| Deployment/cloudflared-watch-oberth-v2 | metadata label app.kubernetes.io/name |
| Deployment/cloudflared-watch-oberth-v2 | metadata label app.kubernetes.io/instance |
| Deployment/cloudflared-watch-oberth-v2 | spec.template.spec.initContainers[name="fetch-token"].args |

Generic chart labels duplicated the explicit connector labels. The correction
preserves connector identity and emits each key once; it changes no script or
current API object. A corrected target must independently prove zero metadata
conflicts. That is expected, not qualified. The sole proposed relinquishment
is the Deployment's legacy `fetch-token.args` ownership leaf. No other object,
label, container field, owner entry or manager is removed.

## Mechanism and qualification boundary

Non-apply updates can edit managedFields; apply bodies cannot set them. The
versioned field-manager code decodes explicit nonempty managedFields on update,
then computes ownership for changed values. This supports investigating a
narrow JSONPatch, not claiming safety on the installed server.
[Kubernetes SSA documentation](https://kubernetes.io/docs/reference/using-api/server-side-apply/),
[apimachinery v0.35.7 code](https://raw.githubusercontent.com/kubernetes/apimachinery/v0.35.7/pkg/util/managedfields/internal/fieldmanager.go).

Remove only one `f:args` leaf from the exact reviewed FieldsV1 trie; change no
spec value. Retain parents, siblings, manager identity, timestamp and all other
entries; refuse an empty resulting entry. Do not apply an incomplete object as
the legacy manager: omission could delete values or alter remaining ownership.
A real fixture must prove persisted subtraction, unchanged spec/generation and
preserved unrelated ownership. Unexpected normalization/transfer blocks the
path; never widen removal to obtain a passing apply.

## Proposed explicit v2 public plan

Use separate strict `oberth.watch-adoption/v2`; never extend
v1 silently or make failed releases generally admissible. Retain four exact
objects and bounded public values. Add creation/expiry (maximum 30 minutes),
namespace and cluster identity, exact target chart/package digest, server image
digest and installer artifact digest. The coordinator independently verifies
the signed successor before producing a concrete plan; self-asserted signing
claims confer no authority.

Each object binds UID/RV, full current and target approved public metadata and
typed spec (including current API defaults), exact complete managedFields and
proposed result. Retain an audited public last-applied annotation unchanged.
Only the connector Deployment may additionally carry its canonical positive
signed-64-bit decimal `deployment.kubernetes.io/revision` and the exact
`oberth.ci/source` value `github.com/oberthci/terraform//k8s/cloudflared-watch`.
Both remain part of the full observed metadata and CAS comparison; the
allowance never permits revising or deleting either during the handoff.
Bind the Helm-rendered target and approved API-defaulted target as separate
public projections: compare preview to the render and successful SSA responses
to the defaulted projection. Do not compare a raw render with a defaulted API
object or silently accept additional defaulted state.
Refuse unapproved annotation values; never persist them or their hashes. Public
specs are reviewed projections, not broad Pod/Helm dumps. No credential value,
private key or value-derived hash belongs in this plan or its receipts.

Require exactly one handoff derived from the named Deployment's full entry: manager
`kubectl-client-side-apply`, operation `Update`, apiVersion `apps/v1`, empty
subresource, fieldsType `FieldsV1`, exact complete current entry and path
`f:spec/f:template/f:spec/f:initContainers/k:{"name":"fetch-token"}/f:args`.
Derive the target entry by one subtraction and compare it to the reviewed
target. Other objects permit no ownership subtraction. Reject shared/unknown
candidate ownership, an atomic owning ancestor, different list keys/version,
duplicate or ambiguous legacy entries, malformed tries and unrelated removal.
Bound strict duplicate-free decoding and allowlist the trie grammar. Preserve
all other reviewed ownership, including controller status; unknown shapes
cannot be interpreted as absent ownership.

Only mode `failed-adoption-recovery` is implemented. It binds the original
plan/receipt, confirmed adoption identities and an exact fresh latest failed
release snapshot. The initial-mode design is deferred and rejected by code.

## Preflight and effect ordering

1. Check release snapshot/expiry. Preflight all four UIDs/RVs, full approved
   metadata, field sets and current public specs before any effect. Obtain the
   bounded no-hooks Helm reuse-values target preview and compare all four
   target metadata/specs. Keep mixed output in memory; sanitize errors.
2. Send all four target objects to typed PATCH endpoints using ApplyPatchType,
   the exact admitted Helm manager, explicit `Force: false`,
   `DryRun: [metav1.DryRunAll]`, `FieldValidation: Strict` and observed UID/RV
   guards. Exclude managedFields from target bodies.
   Preserve apiVersion/kind, metadata.name/namespace and the precondition
   metadata.uid/resourceVersion in those bodies; only unrelated server-managed
   metadata is excluded. Match
   structured status causes: three successes and only the planned Deployment
   args conflict. Missing, additional or unreadable causes fail closed. The
   historical six-conflict set is not permission for a current operation.
3. Dry-run the proposed Deployment metadata JSONPatch and verify its reply.
   Recheck release/expiry, then actual CAS tests UID, RV, full approved metadata
   and the complete managedFields array before removing only the indexed
   legacy args leaf. Escape each JSONPointer component; select the index from
   the exact entry, never a presumed position. No patch writes under spec/data.
   Initial adoption retains existing four-object ownership CAS checks/receipts;
   the args subtraction remains separately explicit.
4. Verify identity/new RV, unchanged public spec/generation, exact subtraction
   and unchanged other owners. Preserve confirmed public receipts on failure.
   An uncertain reply stops without retry, rollback or automatic continuation;
   a fresh reviewed plan is required.
5. Re-read all four at confirmed post-CAS RVs. Repeat real Force=false
   SSA DryRunAll: all four must pass, with defaulted public responses matching
   the reviewed target. Recheck all current objects/release/expiry immediately
   before normal supported Helm. Actual argv has no take-ownership,
   force-conflicts, replacement or client-side bypass.

After a confirmed handoff, a dry-run, readback, reply-validation or Helm failure
must return the persisted confirmed public metadata receipt and stop. Receipt
retention applies even when no new Helm revision is created. No rollback or
automatic retry follows any such failure.

Dry-run handoff does not persist state for a later SSA dry-run. The prospective
sequence must first pass on an isolated real API with persisted fixture
handoff; a live dry-run cannot prove the future sequence. UID/RV CAS is per
object; revision/expiry reads are guards, not a cross-object transaction.
Exclusive coordinated live admission is required; observed concurrency stops
the operation. Do not promise atomicity against arbitrary administrators.
The former client-side writer must be quiescent throughout this cutover. Later
drift remains detectable and refused; the handoff does not eliminate that
external writer's authority or prevent it from acquiring the field again.

The exact installed Helm 4.2.3 module source was independently read and hashed:
`pkg/kube/client.go` SHA
`64c4eba24db39faa1ad0be59c8416dcd8802121ba173b0aa31729fd62fc9c371` and
`pkg/action/upgrade.go` SHA
`12e6370cdcba06c2d0b088eb22c1d80db8837890d7919ce40d3023961a7b3e6b`,
under `/var/tmp/issue554-shared-fixture/trivy-build/gomodcache/helm.sh/helm/v4@v4.2.3`.
Upgrade's dry-run returns before its KubeClient.Update call. The optional CSA
upgrade uses only the current manager name, not kubectl-client-side-apply.
Normal SSA sends an explicit force boolean and uses the executable basename
absent an explicit manager. This explains why the old Helm preview did not
qualify the actual conflict path; it remains source evidence, not a fixture
result. Hash-bind the exact installed binary and invoke it under its expected
name so SSA preflight uses the same manager as normal Helm.

## Exact failed revision 72 recovery

Keep the original expired plan immutable, SHA
`11cdaed77f997adc5393ec79517c55f7dba2fc4a282150c088ac99c37eefb203`.
Bind original failure receipt
`a9c74791b7d763ecb4e2491af18f69e5009454760ba6e585afe377712f13791d` and
prior public state
`751ad7ce7bb8ddbeb70b710fe07cac0ef0ff9ebf5e65545c91380e975d1696f8`.
They identify the attempt, not fresh RVs or permission to replay.

A fresh public snapshot must show latest 72 failed, chart oberth-0.16.25,
its history relationship to prior deployed 71, the same four confirmed UIDs
and current Helm ownership. Refuse newer/pending/unknown operations; never
silently choose deployed 71 over latest failed 72. Bind the failed release
record UID/RV and bounded public identity/status without persisting or hashing
mixed release values. Read again before each effect and normal Helm. The
successor creates an ordinary new revision, never rewrites 72, fabricates
deployed state or resets history. The old ready server/connector proves no
rollout success.

## Prepared real regression — unrun

Use an isolated real API matching the target version and admitted Helm. Fake
clientsets cannot qualify ownership or SSA. Seed a minimal Helm release and
four synthetic connector objects via actual client-side apply under
kubectl-client-side-apply, including older public fetch-token args. Use
synthetic trust, no production kubeconfig/namespace or real credentials.

Recreate the original supported v1 four-object CAS then normal Helm's six
conflicts and latest failed revision. Preserve this attempt. Corrected chart
SSA must reduce it to only args. Exercise reviewed failed-forward recovery,
persist the metadata-only handoff, and prove four post-CAS SSA dry-runs pass
without spec/generation/pod-template mutation. Ordinary Helm must converge at
the next revision on the same four UIDs and exact public target. Prove no
deletion/recreation, blanket owner loss or credential output.
Check ordinary Helm's resulting ownership: it must acquire the intended field
without reintroducing lost ownership or removing unrelated remaining fields.

Prepare negatives for drift on each object before first CAS; replacement/RV
movement afterward; shared/unknown/ancestor candidate owners; unrelated
subtraction; alternate API version/subresource/operation/FieldsType; multiple
matching entries; empty/foreign paths; malformed/duplicate tries; full-entry
drift; annotation/target drift; expiry before
each effect; release revision/status/history movement; partial CAS and uncertain
reply; unexpected defaulted SSA response; and extra conflicts. Each stops at
the guarded point, retains confirmed receipts and prevents unqualified Helm.
The original expired plan must fail unchanged. None of these checks has run.

Implementation wiring and the division between real effect-engine proof and
production signed admission proof are listed in
[the test catalog](watch-csa-test-catalog.md). The isolated fixture calls the
same unexported engine with owned synthetic history and UIDs; it cannot bind
the immutable original production UIDs or a not-yet-released successor binary.
Production always verifies those origin and artifact guards before entering
the engine. No CLI flag exposes the seam. The actual released-installer path
and coordinated live forward recovery remain separate required evidence.

The recovery API client and sealed Helm config now derive from one new,
validated literal HTTPS configuration. Impersonation and caller transport/auth
hooks are rejected, file-backed CA/cert/key input is frozen once and removed
from both configurations, and ambient proxy input is blocked. The signed
running installer is bound through the kernel `/proc/self/exe` descriptor.
Approved defaulted targets bind complete non-fieldset metadata; finalizers,
owner references, generateName/selfLink and deletion-grace input are refused.
Current observations bind that metadata at the current UID/RV/generation.
Handoff replies compare the complete actual baseline, normalizing only the
confirmed RV and exact prospective fieldset; SSA replies bind complete
defaulted metadata with only its expected current RV/prospective fieldset
normalization. The legacy index is derived from the validated observed array,
so an equivalent reordered public plan cannot select another owner entry.


## Public compatibility values and API binding

The strict v2 plan requires `values_sha256`, a lowercase SHA-256 digest of the
canonical merged public values JSON (sorted object keys, no trailing newline).
Only v2 additionally admits complete `compatibility` identities `schemaIdentity`,
`auditDomain`, and `witnessKeyInfo`, and a complete `argo.goProxy` namespace with
`modulePrefix`, `repositoryPrefix`, `upstream`, and `organization`. The daemon's
protocol, witness and module-namespace validators check these exact strings.
No proxy enablement, port, image, credential, RBAC or other policy override is
admitted. Partial, unknown, null, duplicate and wrongly typed fields refuse;
conflicting namespace values in multiple files refuse. Existing deployment
identifiers are supplied by the administrator outside the source checkout.

The installer seals the canonical merged bytes, then independently re-reads,
revalidates and compares that sealed digest before artifact preparation or API
effects. Helm receives only that descriptor. Replacing the original values
path cannot alter the approved install. V1 retains its connector-only whitelist.
Unit tests separately cover each of the seven fields, digest spelling and type,
whole-group omission, conflicting files and source-path replacement.

The qualified API patch set is exactly 1.36.2 and 1.36.3. Every public plan records
the full observed API GitVersion, and every effect boundary checks that exact
string again. The owned fixture tools independently bind each version's actual
API-server binary and expected reported version; Helm remains pinned to 4.2.3.
Fixture qualification proves the private effect engine under synthetic history;
it does not prove the signed released-installer preparation or live rollout.
