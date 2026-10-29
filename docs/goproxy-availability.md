# Go module proxy availability

The private mirror has its own TLS listener on the Oberth pod. A pod can be
Running with that listener open while `/readyz` reports pending scheduler
recovery, failed database/configuration checks or a stale serving snapshot.
With a single endpoint, a readiness-gated
Service then has no usable backend: kube-proxy can reject connections before
any HTTP request reaches Oberth. A local request can work at the same time that
a pipeline sees `connect: connection refused` through the ClusterIP (#1204).

The goproxy Service sets `publishNotReadyAddresses: true` so already-running
pipelines can keep reading mirrored source during these dependency failures.
Only the private, read-only proxy Service bypasses pod readiness. The API/SSH
Service still respects startup recovery, database/configuration readiness and
shutdown drain. Kubernetes API availability and periodic audit health remain
status diagnostics; operation-level mutation gates, TLS verification, namespace
ownership and NetworkPolicy boundaries remain enforced. The listener is still
owned by the server process and closes during shutdown. Publication is not a guarantee that
a pod starting up or shutting down already has a usable listener.

Readiness, liveness and startup probes have explicit five-second timeouts. The
previous implicit one-second deadline included TLS handshake and CPU scheduling,
not just handler execution. Readiness dependency checks already run in a bounded
background snapshot; `/readyz` does not perform those checks inline.
A five-second timeout gives a loaded process more scheduling time; it does not
turn a genuine 503 into readiness. A Running pod and a single successful local
probe are insufficient evidence that the Service stayed routable throughout a
build.

For a refusal, correlate the step's timestamp with the Service's EndpointSlice
conditions, pod Ready transitions and probe events. Check the listener inside
the pod and from the pipeline namespace using the configured CA, and inspect
both ingress and egress policies where NetworkPolicy is enabled. A TCP refusal
before accept cannot produce a proxy HTTP request log. Listener start/stop and
TLS/HTTP server errors are prefixed with `goproxy`; handled request failures emit
a fixed error class and status, method and bounded quoted path. Each handler
logs the first failure of each class immediately, then at most once per class
per 30 seconds. The next emitted failure includes a `suppressed` count of
intervening failures; a quiet class has no periodic summary. Limiter state uses
fixed class windows, independent of request paths. Normal public 404 misses are
expected resolver fallthrough and are not logged as failures.
Queries and raw private-backend errors are not logged.

Retained and live step logs add one attributed `oberth: Go module proxy
unavailable (connection refused)` diagnostic when Go reports a refusal from the
configured mirror. The generated hint, attribution prefix and newline count
toward the existing 32 MiB per-step transformed-output limit and the aggregate
run-log limit in both live and replay paths. The hint can bring truncation
forward by its encoded length; command verdicts retain their existing rules.

There is no automatic replay of arbitrary build steps or stderr-based retry.
A step can publish artifacts, perform other effects, or report both a refusal
and a checksum failure. Such output cannot safely authorize another execution.
The resolver chain retains its comma after the private mirror: replacing it with
a pipe would allow private transport failures to fall through to public
resolvers. TLS errors, checksum/integrity failures and private HTTP denials
remain terminal. A caller implementing its own dialer may bound retries with
backoff for `ECONNREFUSED` before any TLS or request bytes are sent; this batch
does not install a new dialer into the Go tool or change its retry policy.

The default concurrency regression runs four real Go download processes with
independent cold caches and a 64-module private graph through the TLS listener
and canonical zip path, without external network access; it skips if the Go
toolchain is absent. This test does not reproduce the Kubernetes readiness
failure. Chart checks pin the proxy-only readiness exception and all probe
timeouts. The #1204 report also records an isolated Kubernetes reproduction
using four cold downloads of Oberth's actual 140-module graph, including the
before/after EndpointSlice and listener evidence.
