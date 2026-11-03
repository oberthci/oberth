# Connector and site ownership handoff

Before upgrading a pre-0.17.83 core to the retirement release, move connector
runtime and website publishing to independently operated resources. The core
chart no longer installs a connector or its PriorityClass, and core release
workflows no longer publish a website. An upgrade must not remove the only
working ingress or publishing owner.

`oberth upgrade --ownership-handoff receipt.json` checks the handoff both during
initial preflight (including `--dry-run`) and immediately before its first live
write. A missing, stale, wrong-cluster or wrong-revision receipt refuses the
upgrade. This is separate from the legacy execution retirement check. Direct
Helm or Kubernetes writes bypass the installer; use this checked upgrade path.

## Move ownership before removal

1. Record the source Helm revision and save its rendered manifest and values in
   a private directory. Keep the current core serving traffic throughout the
   handoff. Inventory the connector Deployment, service account, ConfigMaps,
   secret references, authentication roles, routing, TLS/origin trust and
   PriorityClass. Identify the repository and credentials that publish the site.

2. Create an external repository/release owned by the responsible team. Import
   the connector configuration into a **different namespace**, using independent
   credentials, authentication bindings and trust material. Manage its entire
   lifecycle there. Recreate dependencies under external ownership; do not just
   rename Helm ownership annotations on existing core resources. The old Helm
   manifest still identifies those objects for removal. A replacement
   PriorityClass must have a distinct name, or omit `priorityClassName`.

   Label the external Namespace, Deployment, ServiceAccount, referenced
   ConfigMaps, Secrets, PVCs and any PriorityClass:

   ```yaml
   metadata:
     labels:
       oberth.ci/external-owner: edge-team
   ```

   Use your independent chart/GitOps tooling to set the label. The value is the
   owner in the handoff receipt. The installer checks the live Deployment UID,
   a completed rollout with positive available/ready/updated replica counts,
   observed generation, and the ownership of referenced volume, projected,
   environment and image-pull dependencies. A default service account must also
   be independently owned. Objects deleting or still present in the source
   core manifest are refused. The check does not adopt or create any objects.

3. Cut over the connector routing under operator control. Verify real traffic
   through the external connector, including origin TLS, authentication,
   reconnects and credential renewal. Keep the old connector available until
   that verification passes. Readiness alone does not prove traffic routing or
   that a script's external credentials are independently managed.

4. Move site sources and publishing workflows to a separately owned repository
   at an immutable commit. Preserve full source history in that repository or an
   archived source bundle; migrate DNS/domain configuration, secrets and publish
   credentials. Publish independently and verify the real site and release
   links. Core source deletions do not delete an already published external
   site, but a separate owner must maintain future publishing.

5. Save the traffic/publish/credential checks in an operator review record and
   hash it with SHA-256. Record the cluster identity, current source revision,
   exact rendered manifest SHA-256 and external Deployment UID. For example:

   ```sh
   umask 077
   kubectl --context "$CONTEXT" get namespace kube-system -o jsonpath='{.metadata.uid}'
   helm --kube-context "$CONTEXT" history oberth -n "$CORE_NAMESPACE" -o json
   helm --kube-context "$CONTEXT" get manifest oberth -n "$CORE_NAMESPACE" \
     --revision "$REVISION" > source-manifest.yaml
   shasum -a 256 source-manifest.yaml traffic-and-publish-review.txt
   kubectl --context "$CONTEXT" -n "$EXTERNAL_NAMESPACE" get deployment "$CONNECTOR" \
     -o jsonpath='{.metadata.uid}'
   ```

   Commands are examples for the administrator to run; product tests use only
   mocked Kubernetes and Helm. Protect rendered manifests because they may
   contain credentials. Hash the raw manifest bytes, including the final newline.

## Receipt format

Write a regular local JSON file, at most 64 KiB. The following is a template;
replace every placeholder with observed values. No deployment name, external
hostname or team is built into the product.

```json
{
  "format": "oberth-ownership-handoff-v1",
  "operator": "responsible-operator",
  "verified_at": "2026-11-03T12:00:00Z",
  "cluster_uid": "observed-kube-system-namespace-uid",
  "source_namespace": "core",
  "source_revision": "7",
  "source_manifest_sha256": "64-lowercase-hex-digits",
  "connector": {
    "mode": "external",
    "owner": "edge-team",
    "namespace": "edge",
    "deployment": "connector",
    "uid": "observed-deployment-uid",
    "traffic_verified": true,
    "credentials_independent": true,
    "evidence_sha256": "64-lowercase-hex-digits"
  },
  "site": {
    "mode": "external",
    "owner": "docs-team",
    "repository": "https://forge.example/team/site",
    "revision": "40-or-64-lowercase-hex-git-commit",
    "url": "https://docs.example",
    "traffic_verified": true,
    "credentials_independent": true,
    "evidence_sha256": "64-lowercase-hex-digits"
  }
}
```

When a function was never used, replace its destination with
`{"mode":"unused","reason":"specific operator explanation"}`. An installed
connector Deployment prevents an `unused` connector declaration. Site usage
cannot be inferred from Kubernetes, so the responsible operator must explicitly
attest that it is unused. An omitted site declaration never counts as unused.

Verification must be less than 24 hours old. The source release/revision and
manifest must still match. If another upgrade changes them, repeat the checks
and produce a new receipt. Preserve the receipt and its referenced evidence in
external operations records; it contains no tokens. It is a trusted operator
attestation of traffic and publishing, not an automated remote-site proof.

Run a dry-run with the receipt, review the rendered core changes, then apply the
same upgrade under the administrator's authorization. Keep the external connector
and site owner operating after the core removes its old resources. Never roll
back by overwriting the external resources with the old core release. Use the
saved configuration and your routing rollback procedure if verification fails;
retain the existing core until a successful handoff is documented.
