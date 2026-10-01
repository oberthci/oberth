# Watch connector ownership

The watch connector uses its Kubernetes identity to read one token from OpenBao
into a memory-backed volume. Its token is never a Helm value. Public OpenBao trust
belongs in `secretstore.caCert`; public HTTPS origin trust belongs separately in
`watchTunnel.originCACert`, mounted at `/etc/oberth-origin-ca/ca.crt`. The tunnel's
origin configuration must refer to that path. A self-signed origin leaf is a valid
explicit trust anchor; neither field may contain a private key.

`oberth install --upgrade --values public-watch.yaml` and `oberth upgrade --set
watchTunnel.enabled=true` support explicit activation. Subsequent upgrades retain
stored enablement through `--reuse-values`. A missing `enabled` key means false;
a present value must be a boolean. The dedicated upgrade command continues to pin
the reviewed cloudflared and OpenBao images and refuses user image overrides.

An existing imperative connector is not automatically adopted. Use an independently
reviewed public `oberth.watch-adoption/v1` JSON plan with
`oberth install --upgrade --skip-argo-workflows --watch-tunnel-adoption-plan plan.json
--values public-watch.yaml`. First run the same command with `--dry-run`. Adoption
is supported only with an existing Linux cluster context. It cannot bootstrap a
cluster or override the published chart or server image. Public values files may
contain only `watchTunnel.enabled: true`, `watchTunnel.originCACert`, and
`secretstore.caCert`. Their bounded, validated contents are frozen in a sealed
memory file for the entire preview and upgrade; changing the original files
cannot change the reviewed operation. The existing network policy is preserved,
and explicit policy changes are refused.

The plan
expires within 24 hours, binds the exact namespace and target chart version, and
contains exactly these four objects:

- ServiceAccount `cloudflared-watch`
- ConfigMap `cloudflared-watch-openbao-ca`
- ConfigMap `cloudflared-watch-oberth-origin-ca`
- Deployment `cloudflared-watch-oberth-v2`

Each object records `kind`, `name`, `uid`, `resource_version`, prior `ownership`
(`managed_by`, `release`, `namespace`), `spec`, and `target_spec`. `spec` is the
reviewed current public state; `target_spec` is the exact public target render.
For the Deployment these are its typed `spec`; for the ServiceAccount they are
`{"automountServiceAccountToken":false}`; for each ConfigMap they are
`{"data":{"ca.crt":"PUBLIC PEM"}}` and optional `immutable`. Kubernetes defaults
present in the current Deployment must be included. Do not make a plan from a
broad unreviewed Pod/Helm dump. The plan's top-level fields are `schema`,
`namespace`, `chart_version`, `release_revision` (the existing deployed Helm
revision), `expires_at` (RFC3339), and `objects`. Field spelling is exact and
required fields cannot be null.

The installer validates every current identity, revision, ownership and public
specification, plus Helm's actual `--reuse-values` server upgrade preview,
before any adoption write. The preview is explicitly `--dry-run=server` with
`--no-hooks`; only this nonmutating preview uses `--take-ownership` so it can
calculate the target before adoption. It never passes that flag to the actual
upgrade. The existing release revision and plan deadline are rechecked before
each metadata effect. It then
changes only Helm ownership metadata with UID and resource-version JSONPatch
tests. It refuses foreign or partial release ownership, scope/spec drift,
duplicate JSON fields, extra objects, private key bundles and expired plans.
No blanket Helm ownership mutation, delete or recreation is used.

If a CAS or its reply fails, the command reports the confirmed public subset and
the object whose outcome is uncertain. It stops without retry or rollback. Retain
that state, inspect the exact identities, and prepare a fresh reviewed forward
repair plan. A Helm failure after complete metadata adoption likewise requires
forward repair; it does not justify removing ownership or recreating resources.
The adoption command cannot install OpenBao, Rekor or an Argo controller.

Helm preview stdout and stderr are bounded as they are read. Mixed release
values remain in process memory; only the four reviewed public object
specifications and adoption receipts are exposed. The connector logs successful
token staging without logging the token or a digest derived from it.
