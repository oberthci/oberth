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

Startup and readiness probes have explicit five-second timeouts; liveness
uses fifteen seconds. The
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

## Immutable version artifacts

The enabled proxy stores generated module artifacts under `<data>/goproxy`
(`/data/goproxy` with the default data root), on the existing server data volume.
It does not fetch from Codeberg during a module request: cold reads use Oberth's
already mirrored bare repository. The producer that maintains that mirror still
has its existing fetch policy. Before this cache, each request listed tags and
then read tag metadata, `git show` or `git archive`; zip requests extracted and
recompressed the tree every time. These operations shared the repository mutex
with other Git work. A backend failure surfaced as a generic 502, including
failures caused by a slow or canceled local command.

The first explicit canonical tagged-version request builds `.info`, `.mod` and
`.zip` together. It first confirms tag membership, resolves one immutable Git
object and timestamp, and uses that object for both file and archive reads.
`.mod` stays byte-for-byte verbatim; `.zip` uses the existing canonical
`golang.org/x/mod/zip` conversion and filtering. A changing tag cannot mix
representations during a fill. Pseudo-versions and branch names remain refused.

Objects are addressed by SHA-256. All objects are synced before publishing an
atomic, no-clobber manifest keyed by mapped repository, module path and version.
A cached explicit version stays pinned across process restarts and tag changes.
Warm reads check object size and SHA-256 before serving, without Git processes,
repository locks or catalog reads. Unknown/unmapped private modules remain
refused before cache access. `list` and `@latest` remain live queries; a cache
cannot determine whether a newer tag exists while Git is unavailable.

Concurrent requests for one missing version share a fill; at most two fills run
at once per handler. Warm reads bypass those slots. Read-only backend work has
two attempts, a 15-second context per attempt and 250ms backoff under a shared
31-second request context, including waiting. Catalog resolution and repository
lock waits now inherit the request deadline. Disk syscalls and existing Git
process-group cleanup are not made interruptible by that context. Backend
exhaustion returns **503 with `Retry-After: 1` and `Cache-Control: no-store`**;
failed fills are not negative-cached. Corrupt/contradictory cache data fails
closed as 500 and is not regenerated from a possibly moved tag.

The Go resolver does **not** automatically retry 503 or honor `Retry-After` as
an automatic HTTP retry policy. There is no injected GOFLAGS retry option.
Automatic recovery for transient Git failures happens in the proxy's bounded,
read-only fill attempts; an exhausted request still fails the Go command. A
caller may rerun a failed read/download command under its own deadline. Do not
replay an arbitrary build step based on stderr. Preserve
`<mirror>,https://proxy.golang.org|direct`: changing the private boundary to `|`
would send private coordinates to public resolution on 503. Go's documented
[proxy fallback rules](https://go.dev/ref/mod#goproxy-protocol) explain that
comma/pipe distinction. Real-Go tests verify successful recovery, go.sum
checks, and that exhausted private failures reach neither public nor direct
resolution.

No migration or additional configuration is required. The cache is derived
private source, not a history backup or an authority database. It has no
automatic expiry or garbage collection: monitor space alongside the existing
Git, log and artifact stores. Interrupted fills can leave unreferenced objects
or temporary files, but no partial published version. To reclaim/reset the
cache, stop/quiesce proxy service and remove its entire derived cache directory;
the next request rebuilds from Git and may be slow. Do not edit manifests or
objects in place. Keep an authoritative copy if repairing corruption; existing
client go.sum checks still reject changed module content after a reset.
