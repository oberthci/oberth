# Metrics

`GET /metrics` on the server's HTTPS port exports Prometheus text: the
readiness sample ([readiness under load](readyz-load-flap.md)), TLS
certificate expiry, token use, admission pressure and the resource sampler.
It answers only an unscoped admin bearer token; a scoped token gets 403.

## Scraping with the Prometheus Operator

The chart renders a `ServiceMonitor` when asked (default off). Nothing else in
the chart changes, and the cluster needs the Prometheus Operator CRDs.

1. Mint an unscoped admin uplink for the scraper alone, so it can be revoked
   on its own ([MCP setup](mcp-setup.md) shows the output), and put its token
   in a Secret in the release namespace:

   ```sh
   kubectl exec -i -n oberth deploy/oberth -- \
     oberth uplink add --admin - metrics@prometheus < metrics.pub
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
| `metrics.serviceMonitor.bearerTokenSecretName` / `bearerTokenSecretKey` | the Secret and key holding the admin token (required) |
| `metrics.serviceMonitor.caConfigMapName` / `caConfigMapKey` | the ConfigMap and key holding the CA (required) |
| `metrics.serviceMonitor.serverName` | TLS server name; empty is `<fullname>.<namespace>.svc`, which the server certificate always carries |

The chart refuses an enabled monitor without the token Secret or the CA: the
scrape always verifies the server certificate. The admin token in that Secret
can do everything an administrator can; whoever can read Secrets in the
release namespace can read it.
