# Container logs

The dashboard lists observed Argo node IDs and Pod container names. Select a
container to read it. Live output is a plain Kubernetes `pods/log?follow`
request; leaving the view closes it. A closed stream stays closed. Use Open log to make another request. There is no automatic reconnect.

When an observed container finishes, Oberth reads its log once and stores a
copy under the run. All copies together share `--run-log-limit-bytes` per run
(default 64 MiB). Capture streams through a small buffer to disk; it does not
buffer a whole container log in memory. Network reads run separately from status
observation and identity handling. At Workflow completion, containers still
running are copied too, or receive an unavailable reason. Truncation, a missing Pod, or an interrupted read gets an
explicit retained reason. Authored PodGC can remove a Pod before this copy;
log availability does not change Argo's run verdict. Existing log retention
continues to remove evidence on its configured schedule.

Authenticated REST readers:

- `GET /api/runs/{run}/containers` lists `node_id`, `container`, `pod_name`,
  `burn`, `step`, `stored`, and any `reason`. Native helper containers are
  hidden from this default list because their logs can contain resolved inputs
  and outputs. They remain readable with an explicit container selector.
- `GET /api/runs/{run}/containerlog?node_id=...&container=...` returns retained
  output with the existing bounded filters and continuation metadata.
- `GET /api/runs/{run}/containerlog/stream?node_id=...&container=...` streams
  plain text. A stored copy is served whenever available, including during an
  active run. `follow=false` requires the retained copy. Each server allows
  32 concurrent streams; excess requests return HTTP 429. Authorization and evidence
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
selection works for new runs through their native node bindings too. The
default CLI and MCP selection reads the selected step's main container; native
bytes are never scanned for old step markers. MCP `logs` accepts the same
native selectors with `repo` and `sha` when a run ID is not yet known.

Ordinary `oberth upgrade` also reviews and fills this absent native default,
rereading operator values immediately before apply. Upgrade the Argo defaults
before deploying the server that stops writing per-Workflow TTLs. With a
server-only chart upgrade or external controller, configure the native default
first; skipping Argo reconciliation can leave completed Workflows without TTLs.

No authored cleanup setting is refused. A terminal Workflow still visible in
the watch cache preserves its Argo verdict even if a live read returns NotFound.
An aggressive authored TTL (including zero) can delete the Workflow before
Oberth or its watch ever observes completion; in that case the verdict cannot
be recovered and the run reports that its result disappeared. Choose a TTL
that leaves time for observation and log capture. Oberth adds no cleanup timer
and does not rewrite the authored value to protect its display.
