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

The first request for a canonical tagged version (including one selected by
`@latest`) builds cheap `.info` and `.mod` metadata. It confirms tag membership and pins one immutable Git object
and timestamp. Only a `.zip` request archives and compresses that pinned object,
then publishes a separate no-clobber zip manifest with the same source identity.
Metadata requests never need a zip or consume archive/extraction IO; an invalid
zip cannot break module-graph metadata resolution. Existing complete manifests
remain readable.
`.mod` stays byte-for-byte verbatim; `.zip` uses the existing canonical
`golang.org/x/mod/zip` conversion and filtering. A changing tag cannot mix
representations during a fill. Pseudo-versions and branch names remain refused.

Objects are addressed by SHA-256. All objects are synced before publishing an
atomic, no-clobber manifest keyed by mapped repository, module path and version.
A cached explicit version stays pinned across process restarts and tag changes.
Warm reads check object size and SHA-256 before serving, without Git processes,
repository locks or catalog reads. Unknown/unmapped private modules remain
refused before cache access. `list` and `@latest` remain live queries; a cache
cannot determine whether a newer tag exists while Git is unavailable. Discovery
omits revoked versions; `@latest` selects the highest remaining release (or
prerelease if no release remains), then serves its pinned `.info`. That mutable
`@latest` URL requires revalidation; it does not inherit explicit versions'
immutable one-year cache lifetime. Unknown
revocation-marker state fails closed rather than advertising that version.

Concurrent requests for one missing version share a fill; at most two fills run
at once per handler. Warm reads bypass those slots. Read-only backend work has
two attempts per stage, a 15-second context per attempt and 250ms backoff under
a separate 31-second shared-fill deadline, including slot waiting. Each HTTP
waiter has its own 31-second deadline; disconnecting or timing out the first
waiter does not cancel work needed by other waiters. A fill may finish after its
last waiter leaves, but is still bounded. Metadata and zip each have their own
fill budget; the HTTP request deadline bounds waiting across both stages.
Catalog resolution and repository lock waits honor the fill deadline. All Git
repository-lock callers now share one fair queue, including ordinary operations;
module readers can leave that queue at their deadline without polling. Disk syscalls and existing Git
process-group cleanup are not made interruptible by that context. Backend
exhaustion returns **503 with `Retry-After: 1` and `Cache-Control: no-store`**;
failed fills are not negative-cached. If a pinned commit has been pruned from
the bare repository, Git confirms its absence with the object protocol and the
proxy returns permanent **500 / `pinned_source_missing`**, without `Retry-After`
or a second internal archive attempt. It never substitutes the moved tag.
Already cached representations remain available; restore the exact missing
object from an authoritative copy to build a missing zip. Do not reset the pin
to resolve this condition. Corrupt/contradictory cache data fails
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

## Tag deletion and per-version revocation

A moved or deleted tag does **not** change an already cached explicit version.
This preserves first-observed bytes and existing go.sum checks; a deleted tag
still disappears from live `list`/`@latest`. Go module retractions are advisory
module metadata, not removal of cached source. A previously unobserved deleted
version is refused. Do not use tag deletion alone to revoke sensitive content.

To deny a compromised version, run the source maintenance utility as the data
volume owner against the initialized proxy cache (no live API or Git needed):

```sh
python3 hack/goproxy-revoke.py --cache-root /data/goproxy \
  --repository forge/owner/repository \
  --module go.example.test/module --version v1.2.3
```

Use the **exact mapped Git input**, full module path, and canonical version.
The utility prints the SHA-256 keyed marker in `<cache>/revoked`. If no cached
metadata entry matches those exact coordinates, it warns on stderr while still
installing a pre-emptive marker. Treat an unexpected warning as a possible typo
or mapping mismatch, not confirmation that an already served version is denied.
The marker returns private 403 for every explicit representation, including warm
hits and cold fills, across restarts and tag recreation. Discovery omits the
revoked version from live `list`/`@latest`. The marker is checked before
work and again before serving; an already streaming response cannot be recalled.
For immediate containment of in-flight responses, quiesce the proxy first.
Markers are administrator-owned policy: retain them during cache reclamation,
and remove one only to deliberately re-enable its original pinned bytes. No
rebind occurs on unrevocation. Whole-cache resets lose both pins and revocations;
copy markers aside and restore them before resuming service. Clients' existing
local caches and already downloaded secrets require separate remediation.

### Running the utility with the scratch server image

The server image is `FROM scratch`: it has no Python, shell or `hack/` directory.
Run the utility from a checked-out release source on the node as the data owner
for a hostPath, or use a separate maintenance pod for a PVC. Do not try to run
Python through `kubectl exec` in the server container.

For a PVC, quiesce/drain the server using the normal operator procedure, record
its replica count and node, scale its Deployment to zero and wait for the old
pod to terminate. Mount the same RWO claim on that node with the **same numeric
UID/GID as the data-volume owner** (chart defaults 65534/65534). Group membership
alone cannot read a 0700 cache. Use an approved Python 3 image **pinned by digest**
and available to that node; the example deliberately requires the operator's
image and installed resource names. It does not change cache ownership or modes.

```bash
set -eu
: "${OBERTH_NAMESPACE:?installed namespace}"
: "${OBERTH_DEPLOYMENT:?installed Deployment}"
: "${OBERTH_POD:?current server pod name}"
: "${OBERTH_DATA_PVC:?server data PVC name}"
: "${PYTHON_IMAGE:?approved Python 3 image pinned as name@sha256:digest}"
case "$PYTHON_IMAGE" in *@sha256:*) ;; *) echo 'Python image must be pinned by digest' >&2; exit 1 ;; esac
# Override these if the installed server/data owner differs from chart defaults.
DATA_UID=${DATA_UID:-65534}
DATA_GID=${DATA_GID:-65534}
REVOKE_REPOSITORY=forge/owner/repository
REVOKE_MODULE=go.example.test/module
REVOKE_VERSION=v1.2.3
server_node=$(kubectl -n "$OBERTH_NAMESPACE" get pod "$OBERTH_POD" -o jsonpath='{.spec.nodeName}')
server_replicas=$(kubectl -n "$OBERTH_NAMESPACE" get deployment "$OBERTH_DEPLOYMENT" -o jsonpath='{.spec.replicas}')
maintenance_pod="oberth-goproxy-revoke-$(date +%s)"
kubectl -n "$OBERTH_NAMESPACE" scale deployment "$OBERTH_DEPLOYMENT" --replicas=0
kubectl -n "$OBERTH_NAMESPACE" wait --for=delete "pod/$OBERTH_POD" --timeout=180s
kubectl -n "$OBERTH_NAMESPACE" create configmap "$maintenance_pod"   --from-file=goproxy-revoke.py=hack/goproxy-revoke.py --dry-run=client -o yaml |   kubectl -n "$OBERTH_NAMESPACE" apply -f -
kubectl -n "$OBERTH_NAMESPACE" apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $maintenance_pod
spec:
  nodeName: $server_node
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: $DATA_UID
    runAsGroup: $DATA_GID
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: revoke
      image: $PYTHON_IMAGE
      command: [python3, /maintenance/goproxy-revoke.py]
      args: [--cache-root, /data/goproxy, --repository, "$REVOKE_REPOSITORY", --module, "$REVOKE_MODULE", --version, "$REVOKE_VERSION"]
      env:
        - name: PYTHONDONTWRITEBYTECODE
          value: "1"
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: data
          mountPath: /data
        - name: utility
          mountPath: /maintenance
          readOnly: true
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $OBERTH_DATA_PVC
    - name: utility
      configMap:
        name: $maintenance_pod
YAML
kubectl -n "$OBERTH_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$maintenance_pod" --timeout=180s
kubectl -n "$OBERTH_NAMESPACE" logs "$maintenance_pod"
kubectl -n "$OBERTH_NAMESPACE" delete pod "$maintenance_pod" --wait=true
kubectl -n "$OBERTH_NAMESPACE" delete configmap "$maintenance_pod"
kubectl -n "$OBERTH_NAMESPACE" scale deployment "$OBERTH_DEPLOYMENT" --replicas="$server_replicas"
kubectl -n "$OBERTH_NAMESPACE" rollout status deployment "$OBERTH_DEPLOYMENT" --timeout=5m
```

If the utility fails, inspect its logs and keep the server quiesced until the
marker is confirmed; the `set -e` example does not resume it after a failed step.
After resuming, verify with the configured CA from a permitted proxy client:

```bash
: "${GOPROXY_CA:?CA certificate file}"
: "${REVOKED_MOD_URL:?complete escaped URL for the revoked version .mod}"
status=$(curl --cacert "$GOPROXY_CA" -sS -o /dev/null -w '%{http_code}' "$REVOKED_MOD_URL")
test "$status" = 403
```

Also check the version is absent from `@v/list` and is not selected by `@latest`.
For pre-emptive denial of a version never served, 403 alone cannot prove the
repository key was correct: unknown private versions also return 403. Verify
its exact mapped Git input separately. Revocation does not require deleting the
tag; deleting a tag alone still cannot revoke pinned artifacts.

The cache has no GC or size metric. Budget for each selected version's compressed
zip plus metadata and temporary extracted/archived trees during two concurrent
fills; warm integrity verification reads each object before streaming it again.
Publication requires filesystem hard links and directory fsync; a full disk or
unsupported filesystem fails cold publication closed (500). Existing directories
are tightened to mode 0700 at startup. Repository removal/remapping makes old
entries unreachable after the handler map refresh at restart, but does not erase
derived source; remove that data under the same quiesced maintenance procedure.
