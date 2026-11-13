# Argo Role ownership migration

Older installations can have `kubectl-patch` (emergency repair),
`kubectl-client-side-apply` (adoption), or Helm's older `Update` operation owning
the atomic `rules` list on the server's `<fullname>-argo` Role. A later Helm 4
server-side apply that changes this list can conflict with that ownership
(#813). `hack/migrate-argo-role` transfers **only this field** to Helm's `Apply`
manager. It preserves the Role, its UID, permissions, metadata, bindings and all
other field ownership. It does not clear `managedFields`, force an apply, delete
or recreate a Role, or change a database.

**The PVC `update` and `list` verbs remain required.** The earlier #1316 RBAC-2
finding that `update` had no caller was incorrect: `SourceSeeder.AdoptClaim`
updates the claim's Workflow owner reference after submission. Orphan cleanup
lists source claims. Removing these verbs would impair storage cleanup. This
migration prepares ownership for reviewed future rules changes; it does not
change the chart's established permission set.

## Run before an upgrade

Use the installation administrator's credentials with access to get and patch
the exact Role. No Role-management permission is added to Oberth's server or
pipeline identities. Run from a checkout containing this tool, with Go matching
`go.mod`. Set every target argument from the installation's actual values; the
pipeline namespace and Helm release namespace can differ. For example:

```sh
go build -o ./oberth-migrate-argo-role ./hack/migrate-argo-role
./oberth-migrate-argo-role \
  --context=my-cluster \
  --namespace=oberth-argo \
  --role=oberth-argo \
  --release=oberth \
  --release-namespace=oberth \
  --dry-run
```

The dry run asks the API server to validate the exact patch with `dryRun=All`;
it does not persist it. After checking that result and the target, run the same
command without `--dry-run`. If configured, supply the existing
`argo.credentialedServiceAccount` using `--credentialed-service-account` and
the chart's fullname override using `--role`. `--kubeconfig` is optional;
`--context` is mandatory. The total request deadline defaults to 30 seconds and
can be set with `--timeout` up to five minutes.

Then run the normal `oberth upgrade` preflight and upgrade. The migration is
explicit, not an automatic chart hook or a preflight side effect. A repeated
invocation against the same shape and Helm-owned rules succeeds without a
write. Fresh installs already owned by Helm also need no change. Do not use
`--force-conflicts` or a blanket managed-fields reset as a fallback.

## Refusals and recovery

The tool checks the Role name, namespace, Helm release name and namespace,
managed-by label, UID, resourceVersion, lifecycle, exact established rules and
supported profile. It accepts rules ownership only from `helm`, `kubectl-patch`
and `kubectl-client-side-apply` using the expected API version and field format.
Unknown rules owners, custom rules, untracked rules or malformed ownership
refuse before any write. Review these cases separately; changing flags to
misrepresent the live configuration is not a migration.

One JSON Patch tests UID, resourceVersion, rules and all original managed fields
before replacing only managed fields. A concurrent edit or Role replacement
fails atomically. The tool never automatically retries a failed write. Inspect
the current Role, then rerun the dry run and migration if the reviewed target
still matches. If the patch succeeded but its response was lost, rerunning reads
the now-migrated Role and returns without another write.

The API's returned object is checked for unchanged content, exact expected
ownership (allowing only entry order and timestamp differences), and repeat
migration being a no-op. An unexpected response returns an error and says to
stop before upgrading. It cannot undo admission-webhook changes; investigate
the returned/live object rather than blindly restoring an older snapshot.
Downgrading the chart does not require restoring legacy ownership: the Role's
rules have not changed.

## Validation and limits

`go test ./hack/migrate-argo-role` exercises both historical chart fixtures,
legacy Update and Apply ownership, shared Helm/legacy ownership, unrelated
owners, custom credentialed identities, stale patches, refusal paths and a
local HTTP API's dry-run/apply/no-op sequence. Kubernetes' field-management
engine reproduces the pre-migration conflict and proves a synthetic later
rules tightening and its repeat can apply as Helm without force. The synthetic
tightening is test-only; it is not the shipped permission set.

The chart and source-claim tests check the fixtures and retained verbs:

```sh
go test ./cmd/oberth -run TestChartArgoRoleRulesFrozenAtV0179
go test ./internal/argojob -run 'TestSourceClaimAdoptionRequiresPVCUpdate|TestSweepCollectsOrphanedSourceClaims'
```

These are local regression tests,
not a claim of migration against a serving cluster. Use the server-side dry run
for the target cluster's admission policies before the explicit migration.

Direct managed-fields editing is exceptional; Kubernetes documents its behavior
under [Server-Side Apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/).
This tool limits that operation to the reviewed Role shape and one ownership
field, with atomic preconditions and response verification.
