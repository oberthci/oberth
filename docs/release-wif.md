# Release Workload Identity Federation

Release federation is disabled unless the server receives an administrator-owned
JSON capability file through `--argo-release-wif-config=/absolute/path.json`.
The server reads the file at startup. Repository documents cannot choose a
provider, audience, Google service account, or Kubernetes identity.

The Helm interface is `argo.releaseWIF` (default `{}`, disabled). A nonempty
value is public authorization configuration, rendered into a read-only server
ConfigMap mount with a deployment checksum. It contains no token or private
key. Unknown fields, duplicate JSON properties, namespace mismatches and
noncanonical repository spellings fail closed. The engine freezes nested maps
at construction; role, namespace and step-identity changes require an
explicitly authorized rollout.

## Runtime repository grants (#1497)

The Helm/flag map is the boot default. An unscoped administrator can change
repository entries without an upgrade or restart through `release_wif_grant`,
`release_wif_revoke` and `release_wif_reset` (CLI `oberth release-wif`), and
read the boot, override and effective maps with `release_wif_list`. Overrides
live only in the server database, so a Helm upgrade never shares an object with
them and cannot conflict; the boot default and an override for the same
repository coexist, and the override wins until reset.

- A grant names a registered, unarchived repository, its per-repo release
  `service_account_name` and a `templates` leaf -> role map. Roles must already
  exist in the boot map. `step_service_accounts`/`step_routes` and any other
  field are refused: dedicated WIF-only accounts are Kubernetes objects that
  only the chart creates. Shared pipeline accounts, any CI identity and
  another repository's release identity (from the live identity maps, even
  when that repository has no WIF entry) are refused before the grant is
  stored. The merged map passes the same validation as a boot map.
- A revoke removes the entry, including a Helm entry and one for a retired,
  unregistered `upstream/org/repo` key (the chart schema cannot express that
  removal with `null`). A reset drops the override so the boot entry applies.
- Every change requires a reason and is audited (`release_wif.grant`,
  `release_wif.revoke`, `release_wif.reset`) with the previous override and
  the repository's `previous_effective` and new `effective` entries (`null`
  when absent), so revoking or shadowing a Helm entry records the Helm grant
  it replaced. Changes go through the audit mutation gate. A committed change
  is published to admission at once, even if the client disconnects;
  `release_wif_list` also re-applies the stored set.
- Each admission pins one effective snapshot for its audit Build and its
  submitted Workflow; the policy digest covers the effective map, so the
  submission audit records exactly which map authorized the run. Release
  preflight reads the same layer.
- On startup the stored overrides are applied again. A stored grant that no
  longer validates (for example, its role was removed from Helm) stays
  inactive, is logged and listed by `release_wif_list`, and never widens
  authority. Revocations always apply.

Roles are administrator-defined DNS labels (1–63 lowercase letters/digits/hyphens,
no leading/trailing hyphen), at most 32 per deployment. The existing `image-writer`,
`image-reader` and `chart-writer` names remain valid with unchanged behavior.
Defining a role does not grant it to any repository. Each use needs an explicit
canonical repository + named template -> role mapping. Unknown, unmapped and
invalid names refuse admission. Provider and target remain pinned public data;
roles must use distinct workload identity pools and target service accounts.
Changing ADC alone must not let a token impersonate another role's target.

For example, the administrator's public JSON file can contain:

```json
{
  "namespace": "pipelines",
  "roles": {
    "artifact-deployer": {
      "provider": "projects/123/locations/global/workloadIdentityPools/deployers/providers/ci",
      "service_account": "deployer@example-project.iam.gserviceaccount.com"
    }
  },
  "repositories": {
    "github/example/project": {
      "templates": {"publish": "artifact-deployer"},
      "step_service_accounts": {"publish": "project-publish-wif"},
      "step_routes": {"publish": {"pattern": "deploy-*", "file": ".oberth/deploy.yaml"}}
    }
  }
}
```

Every dedicated account also requires `step_routes.<template>` with the exact
`file` and `pattern` of an existing `argo.releaseTagRoutes` entry for that repo.
For the example above configure `deploy-* -> .oberth/deploy.yaml` separately.
Both the selected route pattern and workflow file must match at admission;
copying `publish` into release.yaml or another route cannot reuse the identity.
Two patterns selecting the same file remain distinct capabilities. To bind a
dedicated identity to release.yaml, configure an explicit route for that file
and its allowed tag pattern; the unmatched default is not an implicit grant.
Read-only preflight checks the file's planned binding without inventing a tag,
and does not authorize delivery. Legacy repository WIF mappings remain unchanged.

Optional `step_service_accounts` pins a dedicated account for an approved
WIF-only leaf. When every WIF leaf is pinned and there are no secret-store consumers, the
workflow and ordinary siblings
use the uncredentialed pipeline identity; the approved leaves alone get their
step accounts. A repository with no secret grants needs no secret-derived
repository identity. Legacy WIF leaves without a pinned account still require
`service_account_name` matching the current server-selected repository release
identity. That field may be omitted when every configured template is pinned. It needs **no dummy secret grant**, no Vault role and
no Vault policy. The chart creates these accounts in the pipeline namespace with
automount disabled and no RBAC bindings. Standalone administrators pre-create
the same pinned accounts; preflight checks them using existing ServiceAccount GET
permission. Missing accounts refuse submission with a configuration recovery hint.

A pinned step account must differ from every repository/shared/CI identity and
any other step's account, including the current grant-reconciler snapshot.
Its template must be a stable top-level container/script leaf explicitly mapped
to a configured WIF role and must not consume the Oberth secret store. Ordinary
siblings do not inherit it. Branch, promotion and schedule runs cannot receive
it, even if they use the same template name or author that ServiceAccount in YAML.
Existing named-secret-grant per-step identities remain unchanged when the optional
mapping is absent. The public policy and nested maps are frozen for the engine's
lifetime; a Helm change requires a rollout.

This interface does not create cloud pools, modify IAM, retire keys or migrate
publisher commands. Cloud trust must explicitly bind each intended role to its
exact `system:serviceaccount:<namespace>:<account>` subject. Review and test that
external authorization before activating a new mapping.

An approved named container or script leaf requests its capability:

```yaml
metadata:
  annotations:
    oberth.ci/release-wif-role: artifact-deployer
```

The request must match the administrator's exact pipeline namespace, canonical upstream/org/repo,
template name, role, and currently selected per-repository ServiceAccount for legacy leaves, or the
explicitly pinned WIF-only per-step account.
Branch runs, unapproved leaves, shared identities, inherited template defaults,
inline templates, DAG roots, container sets, artifacts, and nonroot mode are
refused. Explicit workflow or leaf token opt-outs remain vetoes.
The release identity must not also name a CI identity or another repository's
release identity. A SHA-256 digest of the public capability document joins the
existing immutable submission-spec/audit binding. It is not a token digest.

The server projects a 600-second audience-specific JWT at
`/run/oberth-gar/token`, and non-secret `external_account` configuration at
`/run/oberth-gar/adc.json` through the Pod's downward API. Only main receives the
readonly mount, `GOOGLE_APPLICATION_CREDENTIALS` and `OBERTH_RELEASE_WIF=1`.
Admission overrides authored values for these variables. Oberth's repository
release helper reads the first-hop URL from the server-owned, read-only ADC
and validates its exact Google IAM endpoint/account syntax. Its final Oberth
target remains fixed by the allowlisted audience. Google enforces first-hop
role permissions; no new source-account environment variable or server rollout
is required. Authored helper
mirrors are resolved before projection, and a server Pod patch removes Argo's
generated wait-container mirror. The Pod explicitly disables automatic token
mounting throughout the WIF-enabled workflow, including ordinary unselected
siblings and their helpers, even if the ServiceAccount defaults to enabling it.
Approved explicit Vault projections remain independently scoped to their
credential-chain containers. The kubelet rotates
the [projected token](https://kubernetes.io/docs/concepts/storage/projected-volumes/).

The projected JWT audience is `https://iam.googleapis.com/<role-provider>`;
ADC uses `//iam.googleapis.com/<role-provider>`, fixed Google STS and IAM
Credentials endpoints, and a file credential source. There is no executable
credential source or automatic fallback to a JSON private key.

Before activation, verify the issuer/JWKS and exact subject inventory against
the deployed cluster, review each pool's audience/subject conditions and target
bindings, and prove successful intended exchanges plus rejected cross-role,
cross-repository, and branch exchanges. Use the deployed issuer and exact configured subject inventory. Offline Pod construction
does not establish any of those cloud authorization outcomes. Keep existing
keys until each migrated publisher class has completed its release acceptance.

This producer unit does not add the historical registry adapter or source-bundle
publisher, change any existing release command, or offer a private-key fallback.
The supported YAML engine invokes `argojob.Build`; the scheduler's existing tag
admission proves reachability against fresh upstream default-branch state before
creating a release run. This feature neither changes that gate nor treats a
repository annotation as reachability evidence. Other supported pipeline formats
and the independently approved secretstore delivery path are unchanged.

### Release declaration preflight

Branch CI compares `.oberth/release.yaml` and every administrator-routed release
workflow at the tested commit with the fresh upstream default. A changed document gets a non-credentialed
`[release-preflight/check]` in the retained run log; warnings do not fail branch
CI. Promotion compares its exact result with its fetched target base and refuses
missing capabilities before publication, including when it reuses green CI.
The check records release declarations for `access_list pending:true`, reports
exact `access_allow` JSON arguments, checks the administrator's `argo.releaseWIF`
map, and reads named-step ServiceAccounts to detect unmaterialized identities.
It requests no tokens and creates no Kubernetes resources. A passing preflight
is not proof of live Vault policy or provider IAM; normal release admission and
credential verification still apply. After an admission refusal, an admin can
use `run_rerun` with `force:true` and a reason after fixing grants (#1098), subject
to its existing bounded tag-rerun policy.

The release preflight also reports each leaf's effective Kubernetes ServiceAccount,
WIF role and full `system:serviceaccount:<namespace>:<account>` subject. A leaf
combining a named-step grant with WIF emits an advisory that cloud trust must
include that per-step subject; the repository subject alone is insufficient.
This advisory does not claim to verify cloud IAM or block a correctly trusted
per-step identity. The access reconciler derives per-step identities from active
grants and publishes them in its identity snapshot; preflight uses that snapshot
and checks account existence rather than relying on a hand-maintained chart list.
