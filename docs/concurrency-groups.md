# Concurrency groups

Repository concurrency groups (`oberth.ci/concurrency-group`,
`oberth.ci/concurrency-limit`), the `concurrency_groups` MCP tool and
`oberth admission groups` are gone. A document that still carries one of the
annotations is refused at admission. A run that must not overlap another declares a native Argo mutex
or semaphore; see
[migrating from concurrency groups](where-limits-live.md#migrating-from-concurrency-groups).
