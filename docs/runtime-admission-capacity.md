# Runtime admission capacity

Oberth's run ceiling (`maxConcurrentJobs`, `--max-concurrent-jobs`,
`--max-concurrent-jobs-file`, `admission.runtimeCapacity`, the
`admission_capacity_config` MCP tool and `oberth admission capacity`) is gone
since v0.20.0. How many pipeline Workflows run at once is the Argo
controller's parallelism; see
[how many Workflows run at once](where-limits-live.md#how-many-workflows-run-at-once).
A runtime override stored by an older server has no effect; the server logs a
warning naming it at startup.
