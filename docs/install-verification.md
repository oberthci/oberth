# Install verification

`oberth install --verify --context <context> --json` reads the advertised client
endpoint saved by install. For example, an install with `--client-host localhost
--client-ssh-port 32022 --client-https-port 32443` saves those choices in the Helm
values `clientEndpoint.host`, `clientEndpoint.sshPort` and
`clientEndpoint.httpsPort`. The chart renders them as the public Service
annotations `oberth.ci/client-host`, `oberth.ci/client-ssh-port` and
`oberth.ci/client-https-port`. These host ports do not change Kubernetes
Service ports or NodePorts.

Verify uses Service discovery for empty fields, saved choices next, and explicit
`--client-host`, `--client-ssh-port` or `--client-https-port` flags last. Rerunning
install without those flags preserves the saved Helm values.

When `deployment_ready` fails, its JSON check includes a safe `reason` code.
`https_tls` and `ssh_reachable` are blocked by that deployment failure and are
not probed. Their remedy is `blocked by: deployment not ready (<reason>)`.
Restore readiness before diagnosing client connectivity.

`GET /readyz` returns `status` and `code`. Ready responses have code `ready`;
not-ready codes are `no_upstream`, `database_error`, `ssh_identity`,
`startup_recovery`, `stale`, `canceled`, `deadline`, `dependencies`, `draining` or
`unavailable`. Codes never include dependency error messages, credentials or
upstream addresses. The process uses its cached readiness sample for requests.

Verify obtains that same cached state through `oberth admin readiness` over the
pod's owner-only admin socket using Kubernetes exec. This works when the unready
pod is absent from Service endpoints. If the process cannot be queried, the check
reports `unavailable` and directs the operator to pod events. The diagnostic
command prints the safe JSON and exits successfully even for a not-ready state.

`oberth install --dry-run` reviews the plan without reserving output paths or
reading recovery credentials. Existing admin certificate and secrets output
files are allowed and left untouched. A real install still protects existing
outputs from overwrite.

A successful `install --plan` NetworkPolicy measurement can be reused once by
the following install for up to five minutes. The private local receipt is bound
to the verified API endpoint and CA, cluster UID, selected context, pipeline
namespace name/UID/labels, node names/UIDs/labels, install namespace and probe
image. Namespace creation/replacement or changed labels/nodes invalidate it.
Every other preflight check still runs. Missing, expired, failed, malformed or
unreadable receipts require the complete live measurement; public plan JSON is
not accepted as proof. An install without a recent successful plan still has
the full fresh-install NetworkPolicy gate.
