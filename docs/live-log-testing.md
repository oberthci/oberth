# Native container log checks

The API race tests cover plain Kubernetes bytes, immediate flushing, client
cancellation, write deadlines and scope/credential revalidation. The server
stream-cap test proves excess connections are refused and completed requests
release their slot. There are no log frames, cursors or automatic reconnects.

`TestNativeCaptureLargeLogMemoryBound` generates 96 MiB without allocating it,
captures a bounded copy, and requires less than 8 MiB of allocations and source
reads at most 32 KiB. Stored stream readers use file sections.
`TestNativeSlowCaptureDoesNotDelayObservationOrRetirement` blocks a kubelet read
while Workflow completion and executor identity retirement must proceed.
`TestNativeFinalCaptureIncludesRunningContainers` exercises timeout, Stop and an
unhealthy watch with containers still running. Native named-reader tests cover
active and terminal runs, marker-looking bytes and the CI failure excerpt.

`TestNativeTTLDeletedWorkflowUsesObservedVerdict` proves that a cached terminal
verdict survives a live NotFound and that deletion before any observation has
the documented missing-result error. Cleanup compilation tests include TTL zero
and authored PodGC without rewriting. Upgrade tests cover absent-only default
reconciliation, explicit null/empty/zero/custom values, rereads and dry runs.

Run the train's fast gate with these focused tests under race. The full CI race
suites remain the final integration gate. These unit checks do not establish
browser layout or deployed proxy latency.
