# Upgrading across execution retirements

Starting with v0.17.83, `oberth upgrade` refuses to proceed while it can still
see a legacy KVM preflight Workflow or an Oberth-owned legacy VM/VMI. The error
names the resource kind, namespace and name. This applies to dry-run previews,
ordinary upgrades, same-version repairs and explicit same-version value changes;
a true no-op does not inspect or alter the release. `--yes` and `--wait-idle`
do not bypass the check.

The CLI checks before preparing changes and again after preview/drain, before
its first live write. It reads the installed pipeline namespace, walks all
Workflow pages, and checks `oberth.ci/trusted-kvm-intent`. It also walks VM/VMI
pages across namespaces, because the historical backend could use a separate
namespace, checking `oberth.ci/tier=vm-runner`, `oberth.ci/vm-spec` and
`oberth.ci/vm-identity`. Unrelated objects do not block the upgrade. Completed
or terminating objects still count until the API confirms their absence.
API/permission/decoding failures are refusals, not empty inventories. An absent
API group is distinguished through discovery from failure to list an advertised
resource. The operator kubeconfig needs discovery and these read permissions;
this adds no server/chart RBAC and performs no cleanup itself.

1. Retain a verified database backup and its recovery key. Keep the existing
   server and controller running. Stop new legacy submissions at their source
   (pushes, release requests and scheduled triggers) during the cutover; the
   CLI's two observations are not a durable admission pause.
2. Let active legacy runs finish, or cancel them through the current server's
   normal run cancellation API. Keep their current grants and cleanup RBAC
   available until deletion is confirmed. Do not replace the server or freeze,
   delete or rewrite execution journals to make this check pass.
3. Inspect each named resource and its owner. For a stuck finalizer or failed
   deletion, restore the controller/API/cleanup permission and let its owner
   finish. An orphan with no recoverable owner needs a separately reviewed
   manual cleanup using its exact name and UID. A terminal run or a cancellation
   request alone does not prove that its external resources are gone.
4. Retry `oberth upgrade --dry-run` after all named resources disappear, then
   perform the normal upgrade while submissions remain stopped. The apply
   path rechecks the inventory. Restart ordinary submissions after verifying
   readiness and recovery.

Schema 47 still preserves the live legacy journals and recovery engine in this
candidate. This gate adds protection at replacement time; it does not complete
Stage 2 removal or migrate a repository's VM-based pipeline. Direct Helm/image
replacement bypasses the CLI guard and must follow the same drain and absence
checks. Move the website/connector to their external owner before using the
separate final connector-removal commit, as described in the
[staged migration](releases/unreleased-execution-retirement.md).
