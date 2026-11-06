# Next release: queued run messages (#1487)

Queued and waiting runs show their peak concurrent pod requests, for example
`needs 9.9 CPU, 16 GiB · host CPU busy`. The line gives one blocker: CPU or IO
pressure, the running-run limit, another visible run, or queue position.
The figures use workflow requests and concurrency, not limits or retired
weighted budgets. Scoped viewers see their own figures; shared counts and
hidden repositories remain private. Unavailable figures are omitted.

Demand lookups share one two-second budget for the whole list. A bounded LRU
keeps successful results by run and SHA without expiry and caches failures
and timeouts for 30 seconds. Queued detail messages use plain wording too.

The priority prefix moved off queued lines; a badge shows low priority, high
priority or priority promotion. Standard runs have no priority label.
The dashboard queued block runs from last-to-start at the top to next-in-line
at the bottom, directly above running rows. The queued filter uses the same
order. Running and finished rows keep their existing order; API/MCP lists
continue to use claim order.
Priority remains in its fields and park-policy fallback details remain in the
run's durable reason and audit, without crowding the queued line. Kubernetes
resource waits retain `pod pending: insufficient cpu (4 CPU requested)`.

This is a local next-release change. Release and owner approval of the live
/runs view are separate acceptance steps.
