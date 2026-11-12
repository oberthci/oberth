# Unreleased

## Server CPU and MCP responsiveness (#1540)

- Event readers seek the oldest and newest retained event IDs instead of
  scanning the whole log for each subscriber wake or poll. Lookup cost stays
  nearly constant as retention grows.
- The chart requests 500m CPU for the server and omits its CPU limit by default,
  allowing bursts under queue and event load. The control-plane PriorityClass,
  memory limit and ephemeral-storage limit remain unchanged. Explicit CPU
  overrides remain supported; clear a saved cap with
  `oberth upgrade --set resources.limits.cpu=null`.
- A repeatable load test holds 64 queued runs, 10 running jobs and eight event
  subscribers while asserting MCP response and event-delivery latency.

See [measurements, limitations and reproduction](../server-cpu.md).
