# Bootstrap acceptance

Bootstrap acceptance has two parts: the automated contract cell in branch and
release CI, and the [manual fresh-machine procedure](fresh-machine-acceptance.md)
run on each release candidate before tagging. Record the candidate identity and
manual report alongside the CI result before accepting the candidate.

The deterministic contract cell blocks in an ordinary rest-test Pod. It verifies
signed fetch behavior, the source CLI's JSON plan and context selection against
local TLS/API fixtures, answers/recovery validation, pinned connect and
CA/settings merge, certificate/invite/add-repo contracts, and captured-output
secret scanning. It uses no cluster or VM. See the
[cell instructions](../hack/bootstrap-acceptance/README.md) for commands and
output custody.

The manual procedure starts from a fresh macOS or Linux machine with a working
local `~/.kube/config` pointing at any suitable Kubernetes cluster. The operator
supplies the cluster and dependencies. It exercises plugin installation,
install/connect over mTLS, a smoke run, a held-run patch/continue round trip,
reboot/unseal recovery, kubeconfig integrity and install re-run, then scans
captured output for secrets before reporting. A macOS VM with colima k3s is one
example; a Linux host and a remote cluster follow the same procedure.

The unused root/systemd fresh-machine harness has been removed in favor of this
manual release check. No disposable runner or additional CI infrastructure is
required. A green deterministic cell is evidence for its contracts; successful
fresh-machine acceptance also requires the completed manual report for that
candidate.
