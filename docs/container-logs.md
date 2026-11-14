# Container logs

The dashboard lists observed Argo node IDs and Pod container names. Select a
container to read it. Live output is a plain Kubernetes `pods/log?follow`
request; leaving the view closes it. A closed stream stays closed. Use Open log to make another request. There is no automatic reconnect.

When an observed container finishes, Oberth reads its log once and stores a
copy under the run. Each copy uses `--run-log-limit-bytes` independently
(default 64 MiB). Truncation, a missing Pod, or an interrupted read gets an
explicit retained reason. Authored PodGC can remove a Pod before this copy;
log availability does not change Argo's run verdict. Existing log retention
continues to remove evidence on its configured schedule.

Authenticated REST readers:

- `GET /api/runs/{run}/containers` lists `node_id`, `container`, `pod_name`,
  `stored`, and any `reason`.
- `GET /api/runs/{run}/containerlog?node_id=...&container=...` returns retained
  output with the existing bounded filters and continuation metadata.
- `GET /api/runs/{run}/containerlog/stream?node_id=...&container=...` streams
  plain text. `follow=false` reads the retained copy. Authorization and evidence
  retention are rechecked during the request. The bearer token belongs in the
  Authorization header.

MCP `run_logs` with `id` and `containers:true` lists the native selectors. Pass
`node_id` and `container` to filter a retained copy. With no explicit window,
it returns the last 200 lines. Continue incomplete bounded pages with the
returned byte offset, retaining the same pattern, context and tail direction.
Live streams use REST. Use `run_logs` with `raw:true` or REST
`GET /api/runs/{run}/logs?raw=true` to read historical run bytes.
Historical logs containing marker-looking lines remain
readable; their marker text is displayed as ordinary output.

Authored `ttlStrategy` and `podGC` reach Argo unchanged. The installer shows
Argo's default TTL in review and supplies `secondsAfterCompletion: 3600` only
when `controller.workflowDefaults.spec.ttlStrategy` is absent. Explicit
operator values, including zero, an empty mapping or null, are preserved.

Use `oberth log RUN --containers` to list native selectors, then
`oberth log RUN --node-id NODE --container NAME` to read a retained copy.
The native CLI view preserves all container lines. Historical `--burn`/`--step`
selection remains available for old stored runs. MCP `logs` accepts the same
native selectors with `repo` and `sha` when a run ID is not yet known.
