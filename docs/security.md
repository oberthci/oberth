# Security boundary

Oberth admits an exact Workflow and steps; Argo and Kubernetes execute them. This small boundary
does not make granted pipeline code trustworthy.

## The six things Oberth protects

1. **Who calls.** Administrators and uplinks authenticate. Each uplink can touch only its
   approved repositories. A public page shell does not make its data public.
2. **What moves the default branch and releases.** An agent promotes an exact tested commit;
   it cannot push the default branch directly. The administrator may move it themselves.
   Releases use the release pipeline from default-branch history. Assisted runs are not evidence.
3. **Who gets credentials.** A step receives a stored secret or release credential through an
   administrator-approved grant for the exact repository, file and step; branch CI cannot reach
   release credentials. Declarations grant nothing, and a step's declared path names are on its Pod.
4. **What was tested.** A run receives the exact admitted commit.
   A later branch update cannot replace it. A pinned extra source has an exact commit too.
5. **Oberth's state.** Runs cannot read or change the server database, keys, credentials or other
   repositories' data. Admission refuses node paths, host namespaces, privileged containers,
   server-owned accounts and protected volumes that would cross that boundary.
6. **Oberth's endpoints.** Git, HTTP and tool requests have bounds and checks against path and
   argument injection. Large or malformed requests are refused with a reason and remedy.

## What this does not protect

Pipeline code can use every granted permission, disclose its secrets or attack reachable services.
Branch code can poison a shared cache; use a separate release cache. Source delivery is no sandbox.
Revocation cannot recover material already read.

Pod defaults are on: no privilege escalation, dropped capabilities and RuntimeDefault seccomp.
Internet egress is open; link-local, RFC1918, CGNAT, IPv6 ULA, cluster and node IPs are closed.
Exceptions: DNS UDP/TCP 53, kube API, server Service ports (SSH too), store and run-namespace Pods.
Use `--egress-allow` for JSON CIDR/port/protocol (`TCP` or `UDP`) exceptions, including LAN mirrors.
`--egress-internet=false` limits internet to that list; `--cluster-cidrs` adds hidden ranges.
Install/upgrade review old -> new rules; `--no-egress` removes policy. Re-review network changes.
A NetworkPolicy-capable plugin must enforce it. Pod defaults use `--no-pod-defaults`.
Install, upgrade and `install --verify` require `pod-security.kubernetes.io/enforce=baseline` or
`restricted` on existing pipeline namespaces. New server namespaces enforce `restricted`.
Existing namespaces retain their labels, shown in review.

## Keys, recovery and trust

Keep profile directories mode 0700 and credentials 0600. Use HTTPS, including localhost. The CLI
checks the saved installation's CA, sends credentials only there and refuses redirects. The bridge
reuses that transport without widening host trust. Browser bearers belong to one origin; sign out on
shared machines. Trust fetched executables only after checking signatures and digests with the
pinned key. Follow [key custody](install.md#keep-the-recovery-files),
[secret recovery](install.md#back-up) and [manual unseal](install.md#unseal-after-restart). Never
paste secrets, tokens, invitations, private keys or recovery values into reports/chat.

## Report a boundary failure

Report boundary failures through the repository's private vulnerability reporting. Include the
version and a minimal reproduction; never disclose credentials or exploits publicly.
