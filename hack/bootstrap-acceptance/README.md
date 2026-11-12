# Bootstrap acceptance framework (#1533)

Status: **partial, blocked**. This probe is not a release acceptance cell and
must not be represented as a green release receipt.

`probe.py` runs the verified plugin fetcher, installer plan, explicit-context
answers installation, all-ok Verify, plugin CA pin/trust merge, and CA-only
HTTPS MCP tools/list and system_status. It isolates HOME and Claude settings,
keeps subprocess output private, scans it against every ceremony value and
encoded/decoded private key material, and deletes all private artifacts on
success or failure. It never installs Helm or kubectl. Python and ssh-keygen
are harness tools in addition to the minimal fetcher's prerequisites.

An operator may run it against a **new disposable cluster** with a configured
throwaway forge and an answers file selecting production OpenBao, assist,
client host/ports and that forge. The operator must provision and verify
pinned Helm/kubectl and the cluster first. A fresh k3d cluster is the fallback
when a VM is unavailable. No existing personal cluster is a substitute.

```
python3 hack/bootstrap-acceptance/probe.py \
  --version v0.21.5 --context <disposable-context> \
  --kubeconfig <private-kubeconfig> --answers <answers.json> \
  --confirm-disposable-cluster
```

The reviewed answers/context are the consent boundary. The script creates a
local SSH identity and passes only its public key to installation. Its private
ceremony and key files never become artifacts. Fetch uses the plugin's fixed
public release origin and pinned key; it has no unsigned candidate override.
The initial MCP probe uses the existing bearer interface in this branch;
certificate-mode enrollment awaits integration of pane 2's mutual TLS bundle.

## Remaining release gate work

The release workflow intentionally rejects privileged containers and hostPath,
and ignores repository-selected service accounts. It cannot provision a
KubeVirt VM or run Docker/k3d inside an ordinary release leaf. Do not widen
those policies or mount the release host's Docker socket to make a test pass.
An operator must supply an isolated disposable VM/runner with an authenticated
provisioning and result channel before this can be connected to the release
DAG. No such runner is configured in this checkout; build hosts are explicitly
off limits during this handoff.

The reviewed private unseal implementation is integrated. An optional
`--reboot-driver /absolute/path` invokes an operator-provided executable with
only the chosen context as its argument, then requires initialized/sealed
OpenBao, runs private unseal and independently requires unsealed status and
Verify green. The driver must reboot only its disposable runner and wait for
that cluster's API and pod exec to return. It is not a VM provisioning or
reboot implementation, and has not been exercised against a real runner.
Use `--openbao-namespace` for a reviewed custom namespace. The unseal CLI
verifies the live OpenBao pod/controller and uses the explicit kubeconfig for
both status checks and recovery. Client endpoints use DNS names, never IPs.

The full cell still needs: a repo-owned pinned VM/k3d provisioning manifest;
a disposable forge and initial repository; the exact-SHA assisted failing push
with events_wait started before it, run_hold_patch/run_continue and passed /
not-evidence assertions; actual reboot and sealed-before/unsealed-after checks;
server/Pod/process-log capture added to the private scan; and a release DAG
success dependency before finalization. These are **not** claimed by this
probe. The no-push handoff rule also prohibits executing its eventual push
round trip here. No release dependency was added that could turn this partial
probe into false end-to-end acceptance.

Run offline assertions with:
`python3 -m unittest discover -s hack/bootstrap-acceptance -p 'test_*.py'`.
They exercise all-ok validation, credential scan boundaries/decoding,
non-disclosing command failures, redirect refusal, and a real local HTTPS
handshake that accepts only the chosen CA; they are not live
installation or reboot evidence.
