# Unreleased: CLI, MCP and dashboard quality of life (#1486)

Dashboard run polling now explicitly requests `GET /api/runs/{id}?view=dashboard`
and omits `Timing` and step `MeasuredUsage`, `MeasuredSizeComparison` and
`Termination` diagnostics. The default HTTP response, `oberth run <id> --json`
and full MCP `run_get` retain them. Use MCP `run_resources` or
`oberth run resources --run <id>` for resource analysis.

MCP `repo_list` now returns compact repository records with one latest run;
run history, probes and grant metadata remain available from `/api/repos`.
A history read failure returns an error instead of suggesting no latest run.
`system_status.queue_cap` uses snake_case keys; legacy `queueCap` remains for
one more release. The `queue_cap_config` input remains `maxNotStarted`.

`OBERTH_CA_CERT` accepts inline PEM, including OpenSSL preambles and a BOM,
or a file path. Certificate read errors do not echo the supplied value.

## Next-release follow-up

JSON API and MCP responses of at least 1 KiB support gzip when requested by the
client. SSE and raw downloads keep their streaming behavior.

MCP `run_logs` and CLI `oberth log` now show the last 200 lines when no pattern,
limit, tail, offset or byte offset is supplied. JSON includes a `window`
explanation; MCP text and the CLI stderr summary explain it too. Explicit
filters retain their behavior. Use `--tail --limit 1000` for a larger CLI tail
or `--limit 0` for the existing bounded full read. MCP uses `tail: true` and
`limit: 1000`, or `limit: 0`, respectively.
