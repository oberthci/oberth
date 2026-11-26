# Oberth installation chart

The installer uses the canonical release verifier and consumes only its checked
`oberth.tgz`. The record supplies the version and two product image digests.
The release workflow stamps the chart version, appVersion, root `imageDigest`
and root `sourcePluginImageDigest` placeholders. Product repositories, native
controller/executor/store image references, `targetSchema`, protocol identity
and audit domain are literals authenticated by that archive.
`TestPackagedChartUsesCurrentStoreSchema` compares these format defaults with
`store.Schema` and `stateport.DefaultProtocol()`; changing either owner without
updating the chart fails the check. Release stamping adds no second format source.
Third-party image references retain their version tags and bind a multi-architecture
OCI index digest in `values.yaml`: controller, executor and store each have one
`image.tag` value to update when refreshing that image. The index supplies Linux
amd64 and arm64. The executor argument and ConfigMap read the same executor value.
The defaults render offline with development tags for the two product images;
a local chart is always identified as unverified in the installation review.
Installation identities are existing Secrets created outside chart ownership.
Rendering creates no private key and runs no command. The pre-delete hook releases
this installation's archive finalizers before removing workloads and namespaces.

The controller reads `runs.parallel` through the parent-owned ConfigMap. Zero
means unlimited. The chart owns no second limit. The native subchart's ConfigMap
is disabled, and both its executor argument and the ConfigMap executor image are
derived from the single `argoNative.executor.image` value. The source endpoint has a separate internal Service;
only HTTPS and SSH have node ports. HTTPS Service port `servicePort` defaults to 8443;
443 is explicit. Its target stays 8443 and `nodePorts.https` selects the node port.
Public mappings belong in the profile.

The controller watches ConfigMaps in its own namespace; they contain public
configuration. The live walk must upgrade `--parallel 4` to `--parallel 2`, keep
the controller Pod UID unchanged, and observe the new limit taking effect for
new Workflows without restarting the controller.

Pipeline resource manifests are applied separately from the chart. Existing
ConfigMaps such as `oberth-sync-limits` are outside the release inventory;
an ordinary chart upgrade leaves them untouched. The upgrade test also refuses
unowned objects incorrectly recorded as obsolete in release history.

`server.json` follows the CLI's configuration and composition types and is
mounted at `/etc/oberth/config/server.json`. The sole configuration flag is:
`/usr/local/bin/oberth serve --config /etc/oberth/config/server.json`.
The chart writes explicit server, pipeline and Argo namespaces, six `files.*`
paths, public endpoints, fixed listeners, protocol, database and store settings,
and the log/schedule/assist sections. `runs.parallel` appears only in the
controller ConfigMap, so changing it leaves the server Pod template unchanged. Each file path names a rendered Secret item.

The runtime object contains the Git path, source/admission durations, source
client burst, default branch, image prefixes, public CA ConfigMap, helper image,
workflow-defaults snapshot, source policy and release routes. Chart values
supply each field explicitly. The helper image uses the same product digest as
the server. Native workflow defaults and their server snapshot come from
`argoNative.controller.workflowDefaults`; their JSON digest is tested against
the native reader. Controller/source inputs under `server.runtime` remain
chart inputs used to build native objects; only the CLI wire fields enter the
server's runtime JSON. The closed schema rejects missing or unknown settings.

The chart mounts `oberth-ca` at `/etc/oberth/ca`, `oberth-ssh-host` at
`/etc/oberth/ssh`, `oberth-forge-key` at `/etc/oberth/forge`, and the public
administrator key Secret `oberth-admin` (`admin.pub`, `push.pub`) at
`/etc/oberth/admin`. Host and forge private keys retain their Secret key `key`.
The server issues and renews its own leaf. Administrator authentication remains
the transport's registered certificate/key binding. Both admin.pub and push.pub
contain authorized-key public bytes matching the locally saved certificate. The administrator Secret
contains only public material. `adminPublicKeySHA256` is a public rollout
annotation input and does not enter `server.json`. The installer publishes all
six configured identity files before applying the server Deployment. The server
reads the CA, administrator, host and forge files through their explicit paths.

`secretStore.caSecret` supplies `ca.crt` at `/etc/oberth/store/ca.crt`.
`secretStore.audience` supplies a separately projected service-account token at
`/var/run/secrets/secretstore/token`; the standard API token mount stays enabled.
The audience defaults to `oberth-store`. Both the writer and step store roles
bind this exact value, emitted as `secret_store.audience` in `server.json`.
The typed store configuration, both roles and the projected token are compared
in one render test. The CLI composes admission and its store client from that
same decoded field.

The store's TLS leaf expires at exactly the installation CA's expiration,
ten years from CA creation. There is no store-leaf renewal loop or Secret
update permission. The store drops every capability, uses RuntimeDefault
seccomp and a read-only root; an emptyDir at `/tmp` supports the upstream
configuration preparation. The controller also uses RuntimeDefault seccomp.
Fixed CPU/memory requests are 250m/1Gi for the server, 100m/512Mi for the
controller and 50m/128Mi for the store. Only the store has a memory limit, 512Mi.
All three use the installation control PriorityClass (1000000, not a global default).
It never preempts CI Pods; a pipeline quota prevents CI from using that class.
A store restarted by an upgrade stays sealed: run `oberth unseal`, then repeat the upgrade.
Controller workflow state and server Git child processes have variable memory
needs; working-set samples cannot establish safe memory limits for either.
The server can write data, `/run/oberth` and `/tmp`; its identity, configuration
and token mounts are read-only. One server replica uses Recreate,
HTTP `/livez` and `/readyz` probes on a separate port 8081, and a 30-second termination grace.
The probe listener has eight connections, 1s header reads and 2s reads/writes; it exposes no API routes.
No Service exposes 8081. The server ingress policy allows only HTTPS, SSH and source ports;
the local node retains probe access through the standard NetworkPolicy node allowance.
TLS handshake failures increment `oberth_tls_handshake_failures_total` instead of emitting one log line per peer.
Both probes have zero initial delay and there is no startup probe. The server
must serve liveness from bind and keep readiness at 503 until startup finishes;
its shutdown must fit within this grace period.

The bundled store starts sealed. The engine initializes it once and installs
its static rules; after a sealed reboot, `oberth unseal` reads the local recovery
key explicitly. The readiness endpoint accepts sealed and
uninitialized states so chart rollout does not require an unseal operation.
Database-only archives exclude store data; installation archives report snapshot inclusion.

Dependencies use pinned upstream archives. Controller and executor image overrides
pin Argo v4.0.16. The release chart digest binds these archive bytes too:

| Dependency | Chart | Application | Archive SHA-256 |
| --- | --- | --- | --- |
| Workflow controller | 1.0.24 | v4.0.16 | `7b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50` |
| Secret store | 0.30.7 | v2.7.2 | `fa4e6f3c1bbe936dc7ba079f1303ea56810af2f8600f046d8d5809860a4f980f` |

The parent schema rejects unknown keys, including nested native configuration.
Operator overrides accepted by the engine are limited to its documented public
settings; native subchart inputs are generated, so operator strings never reach
the dependency charts' template-evaluation options. Native CRDs use the bundled
minimal definitions rendered as ordinary templates, including on upgrade. No
CRD download job is rendered. C4 validates submitted workflow schemas.

Native task-result access and admission follow the sole C5 installation handoff.
`oberth-task-results` grants only create/patch through the pipeline namespace's
service-account group. The policy checks the bound Pod extra and both current
and old Workflow labels, and its Deny binding selects only that namespace.
Pod and Pod-status updates do not match its resource rules. The pipeline Role
for the server supplies workflow, Pod, log/exec, account, CronWorkflow and
WorkflowTemplate-read access. Source-token authentication keeps its separate
token-review permission.

The chart creates no executor account or token Secret and sets no executor
account, kubeconfig environment, or token-volume override. C5 supplies each
Workflow's account and Pod-bound projection. Repository/grant owners create the
named accounts; they do not exist as a shared chart default. Native controller
RBAC lets the controller read those accounts. Failed Pod restart is enabled
with exactly one restart.

Render tests compare the Role, binding and guarded policy directly with
`internal/argojob/argojobtest` on main. The contract note grants only that test
import. The frozen owner fixture also records its source and canonical objects.

The source image is registered as the pinned controller's native artifact
driver `oberth`. The controller ConfigMap includes its image entrypoint index
and public endpoint, TLS server name and CA environment. `oberth-source` carries
the same image reference and public trust. The socket driver uses the artifact-driver protocol;
the HTTP task plugin's `ExecutorPlugin` object is a separate native interface.
The source binary requires a token-file-only `KUBECONFIG`; its per-Workflow
identity binding remains a source/native-owner integration requirement. The
chart supplies no shared executor credentials, and render tests claim no live
source-loading result.

The composition review's twelve chart requirements are enforced before render:
typed fields include explicit false/zero values, complete runtime settings and
required identity paths; unsupported configuration is refused; database paths stay under
`/var/lib/oberth` after cleaning; public/store URLs use HTTPS without credentials,
query or fragment. `server.json` is at most 1 MiB in bytes. A quoted ConfigMap
scalar preserves its exact JSON bytes without an extra file newline. Native
JSON serialization emits one value per key.

The Service is named `oberth`. Source delivery connects through the separate
internal `oberth-source` Service and verifies `oberth.<namespace>.svc`, one of
the server's served leaf names. No `.svc.cluster.local` leaf name is assumed.
At install/upgrade the installer reads the CA Secret; its public certificate
feeds `checksum/ca`, so a newly observed valid CA rolls the Pod when applied.
Private CA material remains outside chart values and history. There is no
continuous Secret watcher: direct changes require another reviewed render and
apply. An installation CA without certificate-signing usage is refused.

The chart tests decode the rendered file against a strict local mirror of the
CLI types and check every configured file against mounted Secret items. The
CLI performs its real-loader integration after merging install. Server startup,
readiness and drain remain server-owned; chart delays do not conceal failures.

Upgrades and rollback use the same verifier and complete chart archive. Rollback
fetches the exact earlier version from the saved record, checks its schema and
protocol, stages a new database file when required, and applies that verified
chart with the native upgrade action. Serialized release history never supplies
chart bytes. Missing dependency files refuse with a remedy to run
`oberth upgrade --version <earlier-version>`. The embedded owner signing pin is
the only trust root; an unprovisioned pin refuses. The verifier-owned fixture
installs a synthetic test key only while the Go test entry runs. Product-caller
and runtime guards keep that entry out of product execution.

Local charts install into a fresh installation or matching local-kind history.
The native release Secret records kind and a public revision/version binding;
any retained verified revision, including failed or pending history, refuses a
local replacement. Empty or partially decoded release payloads refuse
provenance. The profile stores endpoint expectations, with no kind/schema
eligibility authority. Install resumes only the latest history version; a
version change uses maintenance. `Release.Render` and fresh local preview work
offline, while existing local/recovery previews check native deletion scope.

Repair marks the exact owned pending revision failed, then upgrades directly
with the supplied fresh chart. Dry preview changes only a private native
history snapshot. Stored manifests and hooks never supply applied target
bytes. Native history remains the inventory and merge base for ordinary
upgrade. Before credential, recovery-record, backup or restore-stage writes,
operation revalidation checks the native deletion set's namespace and live
release ownership. Native apply retains its Update fence for later changes;
this is not a transaction across credentials, storage and resources.
Maintenance uses the live schema floor. Dedicated rollback authenticates its
earlier chart and restores the matching backup before selecting it when
schemas differ.

Backup and restore remove `.restore-*` plaintext leftovers before starting;
dry runs leave local files untouched. A restore keeps the current database
and switches the server to the reviewed new destination. A pending store
bootstrap explicitly reviews reuse of its saved root token and then revokes it.
A sealed store names manual unseal; a rejected unseal key names this profile's
recovery file. If initialization lost its reply before saving either key file,
the refusal names the store data PVC to delete before installing again.

Run egress defaults to internet open, internal closed. DNS UDP/TCP 53, the kube API,
all server Service target ports (including SSH), bundled store and run-namespace Pods
are allowed. Internet CIDRs exclude link-local, RFC1918, CGNAT, IPv6 ULA, cluster
Pod/Service ranges and node/API addresses. IPv6 internet is enabled for IPv6 clusters.
The installer discovers API Service/backend addresses, node addresses and allocation
ranges into `egressNetwork`; direct chart renders must supply those cluster facts.
`policies.clusterCIDRs` adds hidden allocation ranges. Network changes require review.
`policies.allow` adds `{cidr: 192.168.1.10/32, port: 53, protocol: UDP}` exceptions.
Set `policies.internet: false` to keep only those and required cluster destinations.
Install/upgrade accept `--egress-allow`, `--egress-internet=false`, `--cluster-cidrs`
and `--no-egress`. Saved `egress: true` gets the new rules, with an upgrade review arrow.
Node image pulls use the node network. The network plugin must enforce policies.
New server namespaces enforce restricted Pod Security; existing namespaces retain their labels.
