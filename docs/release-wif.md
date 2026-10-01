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
at construction; configuration changes require an explicitly authorized rollout.

This source interface is staged. It does not create cloud pools, change IAM,
enable providers, install configuration, retire existing keys, or migrate
publisher commands. Each of those requires its own reviewed rollout. Keep it
separate from the first release that repairs publisher runtime isolation.

The public configuration has two maps:

```json
{
  "namespace": "oberth-argo",
  "roles": {
    "image-writer": {
      "provider": "projects/138624361369/locations/global/workloadIdentityPools/oberth-release-images/providers/tuxbox",
      "service_account": "image-publisher@example-project.iam.gserviceaccount.com"
    }
  },
  "repositories": {
    "github/oberthci/oberth": {
      "service_account_name": "oberth-argo-github-oberthci-oberth-2b5c2901d41d",
      "templates": {
        "release-publish-images": "image-writer"
      }
    }
  }
}
```

The other roles are `image-reader` and `chart-writer`. Each configured role must
use a distinct workload identity pool and target service account. The staged
Terraform contract uses pools `oberth-release-reader` and
`oberth-release-charts`, each with provider `tuxbox`. Merely selecting another
target in ADC does not restrict a token's authority: IAM must bind each target
only to the exact subjects in that role's pool. Shared-pool subject aliases
would defeat this boundary. See Google's [federation identity guidance](https://docs.cloud.google.com/iam/docs/best-practices-for-using-workload-identity-federation).

An approved named container or script leaf requests its capability:

```yaml
metadata:
  annotations:
    oberth.ci/release-wif-role: image-writer
```

The request must match the administrator's exact pipeline namespace, canonical upstream/org/repo,
template name, role, and currently selected per-repository ServiceAccount.
Branch runs, unapproved leaves, shared identities, inherited template defaults,
inline templates, DAG roots, container sets, artifacts, and nonroot mode are
refused. Explicit workflow or leaf token opt-outs remain vetoes.
The release identity must not also name a CI identity or another repository's
release identity. A SHA-256 digest of the public capability document joins the
existing immutable submission-spec/audit binding. It is not a token digest.

The server projects a 600-second audience-specific JWT at
`/run/oberth-gar/token`, and non-secret `external_account` configuration at
`/run/oberth-gar/adc.json` through the Pod's downward API. Only main receives the
readonly mount and `GOOGLE_APPLICATION_CREDENTIALS` variable. Authored helper
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
cross-repository, and branch exchanges. The staged issuer is
`https://kubernetes.default.svc.cluster.local`; use the current Terraform source
for the exact inventory rather than a historical count. Offline Pod construction
does not establish any of those cloud authorization outcomes. Keep existing
keys until each migrated publisher class has completed its release acceptance.

This producer unit does not add the historical registry adapter or source-bundle
publisher, change any existing release command, or offer a private-key fallback.
The supported YAML engine invokes `argojob.Build`; the scheduler's existing tag
admission proves reachability against fresh upstream default-branch state before
creating a release run. This feature neither changes that gate nor treats a
repository annotation as reachability evidence. Other supported pipeline formats
and the independently approved secretstore delivery path are unchanged.
