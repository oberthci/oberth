# Install and upgrade reruns

A rerun preserves the installed release's settings unless an explicit flag or values file changes them. Release lookup includes pending and failed revisions. A failed lookup or invalid Helm response stops the operation before it can be treated as a new installation.

Before applying an existing Oberth release, the installer renders a server-side Helm preview and prints the sorted paths of changed values. Values themselves are omitted because they can contain credentials. An unreadable live configuration or a preview without its planned configuration stops the operation. Forward repair compares against the last deployed revision, even if the latest revision failed. OpenBao and controller changes follow the preview.

- `--network-policy=auto` chooses a default only for a new release. On a rerun it preserves the live value and reports a disagreement with cluster detection. Use `--network-policy=true` or `false` to change it. If node access fails and the cluster type is unknown, a fresh install requires an explicit choice.
- `service.type` remains unchanged on reruns unless `--service-type` is supplied. Fresh installations default to `NodePort`.
- `promotion.grantMaxLifetime` comes from reused values or an explicit values file (`oberth upgrade --set promotion.grantMaxLifetime=...` also works). The chart and server provide the 24-hour default when the value is absent.
- An explicit `--argo-vault-credentialed-role` wins over the managed default. Existing roles remain in reused values. If an identity producer fails or returns no identities while the live release carries per-repo or per-step identities, provisioning stops rather than reverting to shared identities.
- An existing push banner URL is preserved. Use `--push-banner-url=https://host/runs` to change it, or `--push-banner-url=` to clear it. TLS certificate names alone do not rewrite the live banner.
- An outstanding witness genesis acknowledgement remains in reused values. Clear it explicitly with `--install-rekor --accept-witness-genesis=` after verifying that the server consumed it.

Explicit settings are reconciled even when the installed chart version matches the CLI. Helm and Kubernetes targeting errors are propagated; an unreadable kubeconfig cannot silently select a different Helm target. Database migrations remain forward-only: repair with `oberth upgrade`, not Helm rollback.
