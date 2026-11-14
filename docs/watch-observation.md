# Run observation through shared watches

Oberth observes Workflows and step Pods through two shared namespace caches.
A run subscribes before reading its first snapshot, rereads on a coalesced
notification, and releases its subscription when observation ends. The
15-second idle timer advances cached identity clocks. An ordinary idle run
makes no Workflow GET or Pod LIST. A failed completion or disappearance
confirmation is retried on the next healthy idle tick; it cannot wait forever
for an object event.

A watch/API outage leaves the native Workflow running. Observation resumes on
reconnect, without charging the outage to executor readiness/read windows.
After initial synchronization, unavailable status includes the outage start in
`HH:MM UTC`. Initial cache synchronization delays recovery/dispatch while the API
and Git remain available. A live Workflow GET supplies the final Argo phase;
a single consistent paged Pod snapshot supplies completion diagnostics.
Token issuance, exec and native Stop retain their live identity checks.

## Disposable live-watch lane

Use the local harness with its dedicated kubeconfig. Check `kind get clusters`
and `uptime` first: wait for other clusters to disappear and the one-minute
load to fall below 14. The fault lane accepts exactly one local kind cluster,
a `kind-oberth-...` context, a loopback API endpoint and its verified
control-plane container. It never uses the shell's default context.

```sh
export OBERTH_ARGO_CLUSTER=oberth-argo-watch
export OBERTH_ARGO_LIVE_KUBECONFIG="${TMPDIR:-/tmp}/oberth-argo-watch.kubeconfig"
kind get clusters
uptime
hack/argo-live-cluster.sh up
GOMAXPROCS=2 go test -race -p=2 ./internal/argojob \
  -run '^TestLiveWatchLifecycle$' -count=1 -v -timeout 15m \
  -args live-watch-faults
hack/argo-live-cluster.sh down
```

Keep one transcript and run the four cases serially once. The harness installs
the SHA-256-verified Argo v4.0.8 manifest and the executor digest from the
engine's canonical pin, then waits for the previous controller Pod to exit.
The ordinary live suite does not enable these faults without the explicit
argument. Every command that accesses the cluster uses the dedicated file and
its explicit context. Cleanup deletes only the named disposable cluster.

The cases measure a 60-second run at two points while it is running, restart
the control-plane container during execution, kill and replace the process
hosting the actual hub/controller before adoption, and request native Stop
while watch reconnects are blocked. The replacement process synchronizes a new
cache, adopts the original UID and completes it without another Create. The
Stop case verifies the Workflow survives and its exit handler runs after
reconnect. Each terminal result is compared with a fresh Argo phase.

The counting transport belongs only to the observer clients. Test control
reads use separate clients. Its request counts include startup and live write
gates; comparison during the quiet execution window proves observation cost
does not grow with duration. Completion adds one Workflow GET and one Pod LIST
for this single-page fixture. Source seed/collector waits are outside this
qualification and are unchanged.

## Operator measurement with ten runs

Use the same cluster, Argo configuration, server resource limits and admitted
Workflow in both builds. Record both commit/image digests. Warm the images and
server first. Submit ten runs through Oberth, with an ordinary step sleeping
for 180 seconds. Verify all ten are running under Argo's configured parallelism
and record their durable run IDs and Workflow UIDs. Keep other activity stable.
Submitting Workflows directly would omit Oberth's run observers.

1. Record an idle window, then a 60-second window with all ten runs executing.
   Collect API request counters at both window boundaries and sample process
   RSS every five seconds. Exclude startup, image pulls, completion and rollout.
2. Compare API request rates by resource and verb with the metrics queries below,
   or calculate cumulative counter differences divided by the window seconds.
   Keep startup LISTs and watch reconnects separately in the transcript.
3. Record the idle and ten-run median RSS, sample range and increase in KiB.
   Confirm the measured PID is the server process in its container and use that
   PID and memory field in both builds.
4. Repeat for the second build with identical inputs. Retain durations, request
   deltas/rates, RSS samples, actual concurrency, reconnect count, build IDs and
   final Argo outcomes. Report the values collected; this procedure supplies no
   assumed performance result.

These counters describe the API server's whole workload. Hold other clients
steady and subtract the idle window; the counting-transport live case provides
an observer-only measurement. The resource/verb labels and counter definition
are documented in the [Kubernetes metrics reference](https://kubernetes.io/docs/reference/instrumentation/metrics/#apiserver_request_total).

```promql
sum by (resource, verb) (
  rate(apiserver_request_total{group="argoproj.io",resource="workflows",subresource="",verb=~"GET|LIST|WATCH"}[1m])
)
sum by (resource, verb) (
  rate(apiserver_request_total{group="",resource="pods",subresource="",verb=~"GET|LIST|WATCH"}[1m])
)
```

For an operator-selected context and the server container's PID, collect the
resident-memory field from its process status. `VmRSS` includes anonymous,
file and shared resident pages; see the [process status reference](https://www.kernel.org/doc/html/latest/filesystems/proc.html).

```sh
kubectl --context "$WATCH_CONTEXT" -n "$WATCH_SERVER_NAMESPACE" \
  exec "$WATCH_SERVER_POD" -c "$WATCH_SERVER_CONTAINER" -- \
  cat "/proc/$WATCH_SERVER_PID/status" > watch-server-status.txt
awk '/^VmRSS:/ {print $2, $3}' watch-server-status.txt
kubectl --context "$WATCH_CONTEXT" get --raw /metrics > watch-window.prom
```

Save samples with their timestamps and use the same scrape/sample cadence in
both windows. The local live lane validates engine observation and recovery;
it does not replace this ten-run server deployment measurement.
