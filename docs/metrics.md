# Metrics

`GET /metrics` on the server's HTTPS port exports Prometheus text: the
readiness sample ([readiness under load](readyz-load-flap.md)), TLS
certificate expiry, token use, controller identity verification and storage/backup state.\nThe controller-verification gauge is `oberth_controller_verification_blocked`;\nno resource-admission pressure or per-step usage sampler metrics are exported.
It answers a metrics-scoped uplink token or an unscoped admin token; any
other token gets 403.

## Metrics-scoped tokens

`oberth uplink add --scope metrics` mints a token that reads `GET /metrics`
and nothing else: every other REST route and every MCP tool answer 403, and
its SSH key never authenticates for git. The `metrics` scope is reserved: it
cannot be combined with another scope or with `--admin`, it never covers an
upstream's repositories, and no upstream can be named `metrics`. Expiry, rotation (`oberth uplink rotate`) and
revocation follow the ordinary uplink rules (`uplinkTokens.defaultTTL`,
`uplinkTokens.maxTTL`); `oberth uplink scope --set metrics` narrows an existing
non-admin uplink to it.

## Scraping with the Prometheus Operator

The chart renders a `ServiceMonitor` when asked (default off). Nothing else in
the chart changes, and the cluster needs the Prometheus Operator CRDs.

1. Mint a metrics-scoped uplink for the scraper alone, so it can be revoked
   on its own ([MCP setup](mcp-setup.md) shows the output), and put its token
   in a Secret in the release namespace:

   ```sh
   kubectl exec -i -n oberth deploy/oberth -- \
     oberth uplink add --scope metrics - metrics@prometheus < metrics.pub
   kubectl -n oberth create secret generic oberth-metrics-token --from-file=token=./metrics.token
   ```

2. Put the CA the server certificate chains to in a ConfigMap there, the
   same CA clients pin (`~/.config/oberth/ca.crt` after `oberth install`):

   ```sh
   kubectl -n oberth create configmap oberth-ca --from-file=ca.crt=$HOME/.config/oberth/ca.crt
   ```

3. Enable the monitor:

   ```sh
   oberth upgrade --set metrics.serviceMonitor.enabled=true \
     --set metrics.serviceMonitor.bearerTokenSecretName=oberth-metrics-token \
     --set metrics.serviceMonitor.caConfigMapName=oberth-ca
   ```

| Value | Meaning |
|-------|---------|
| `metrics.serviceMonitor.enabled` | render the ServiceMonitor |
| `metrics.serviceMonitor.interval` | scrape interval |
| `metrics.serviceMonitor.additionalLabels` | labels your Prometheus selects ServiceMonitors by (for example `release: prometheus`) |
| `metrics.serviceMonitor.bearerTokenSecretName` / `bearerTokenSecretKey` | the Secret and key holding the metrics-scoped token (required) |
| `metrics.serviceMonitor.caConfigMapName` / `caConfigMapKey` | the ConfigMap and key holding the CA (required) |
| `metrics.serviceMonitor.serverName` | TLS server name; empty is `<fullname>.<namespace>.svc`, which the server certificate always carries |

The chart refuses an enabled monitor without the token Secret or the CA: the
scrape always verifies the server certificate. Put a metrics-scoped token in
that Secret, never an admin token. The Prometheus Operator copies it into the
configuration Secret of every Prometheus that selects this monitor, so whoever
can read Secrets in the release namespace or that Prometheus's namespace, or
exec into its Pod, holds it too; with the metrics scope that reaches only the
metrics text. The token expires like any other uplink token
(`uplinkTokens.defaultTTL`); the scrape then fails with 401 until the Secret
holds a new one.
