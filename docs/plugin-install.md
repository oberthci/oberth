# Plugin install and repository connections

## Optional connection settings

Install `oberth@oberthci` before a server exists. Leave `url` and `ca_pin`
empty; `/oberth:install` records the installed connection settings. Both
manifest options explicitly have `required: false` and an empty default.
Claude Code can still report unset optional values. Continue installation;
never supply a placeholder endpoint or trust an invented CA pin to hide the
notice. The plugin registers no MCP endpoint before configuration.

Claude Code's [manifest reference](https://code.claude.com/docs/en/plugins-reference#user-configuration)
documents optional values and defaults. An empty default does not guarantee
that every Claude Code version suppresses its unset-options notification.

The identity directory is
`${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/current/`. For executable shell
commands, use `${CLAUDE_CONFIG_DIR:-$HOME/.claude}/oberth/current/client.csr`.
The plugin and CLI must use the same Claude configuration directory.

## Client ports before install

After selecting the context and advertised hostname or IP, save the **public**
`install --plan --json` result to a new file. This contains cluster identity
and checks, never installer secrets. Before the install Review, run:

```sh
sh "$CLAUDE_PLUGIN_ROOT/scripts/install" ports --plan-file '<public plan JSON>' --client-host localhost
# Equivalent CLI:
oberth client ports --plan-file '<public plan JSON>' --client-host localhost
```

Clients may use a DNS name or an IP covered by the leaf certificate's matching
SAN. CA pinning and certificate verification remain required; see
[client certificates](client-certificates.md).

The result proposes `install_flags` with the HTTPS and SSH host ports. It
uses exact node identities from that plan to find a matching local Docker
cluster's published TCP ports, including k3d's serverlb and direct node
mappings. For example, mappings from 30443 to 32443 and 30022 to 32022 propose
`--client-https-port 32443 --client-ssh-port 32022`. Explicit client port flags
override discovery. Pass the selected `--service-type` when it is not NodePort.

Docker is optional. Other clusters get the same TCP probe of their chosen
address; no cluster type controls install admission. Remote Docker daemon
bindings, unrelated clusters, other host interfaces and ambiguous mappings
are not silently selected. `needs_action` can record a DNS mismatch while both
TCP probes succeeded. Describe the DNS remedy and each TCP observation
separately. A failed TCP probe means that port did not answer; it does not
establish a missing route. If access still fails after install, review routing
and offer a routable name/IP or port-forward, then repeat with its host ports. The helper never
creates a port mapping or starts a port-forward itself.

A fresh server may not be listening. A TCP connection or a published port
mapping proves neither Oberth readiness nor TLS trust. Include the proposed
ports in the same user-approved install command, then run install verification
afterwards. Preserve the public install result, including `server_url`,
`git_url` and `ca_sha256`, for server-bound repository setup.

## Repository SSH binding

`/oberth:add-repo` previews `oberth client repository` through its narrow
helper. Supply the selected public install result, the server's registered
`git_path`, and explicit local CA, TLS certificate/key and SSH identity paths.
Only paths enter chat or argv; keys are read by the CLI/OpenSSH, never printed.
No ambient bearer credential or global `oberth` SSH alias selects the server.

The CLI verifies that the public CA matches the install pin and reads
`system_status` and `repo_list` over mTLS with redirects refused. It pins the
server's actual SSH host public key and takes the canonical Git path from the
registration. It prints the exact `push_url`, `server_id`, `ssh_config`, and
`core_ssh_command`. Preview writes nothing; `--consent` creates the binding and
updates only the selected repository. Choose `remote`, `push-url`, or an
explicitly reviewed `replace-remote` for a conflicting existing binding.

The server ID hashes the CA pin, advertised SSH endpoint and SSH host-key
fingerprint. Files live under
`${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/<server-id>/ssh_config` and
`known_hosts`. The repo-local `core.sshCommand` uses `ssh -F <that file>` with
strict host-key checking, the selected local identity and a pinned host-key
alias. Each server gets a distinct immutable config; another install cannot
redirect existing repositories by changing the global `oberth` alias.
Different existing binding files are refused. Clear ambient `GIT_SSH_COMMAND`
or `GIT_SSH` overrides and matching Git URL rewrites before configuring.
Explicit command/environment overrides of Git's transport remain operator
choices outside this binding; the helper clears SSH overrides for its smoke push.

Use `--assist` only when assisted pushes were selected. It adds one repo-local
`push.pushOption=oberth.assist=1`, preserving other local options. No global
Git or SSH configuration is written. For an intentional forge push or an
unassisted evidence run, use a one-command `push.pushOption=` override.

## Custom SSH forges and evidence

`inspect` accepts home-relative origins such as
`ssh://git@forge.example.test:2222/~/org/project.git`, preserving `~/org` in
the upstream base URL. Unknown hosts use `forge=other`; explicitly choose
GitHub, GitLab or Codeberg only when their API is known. Other forges use
manual public-key registration with the exact page or authorized_keys location
supplied by their administrator. No settings URL is invented. Unknown SSH
hosts still require an authoritative fingerprint; `prepare-upstream` accepts
it after `--consent`. A scan never establishes the pin.

An assisted run is not promotion evidence. After an in-place fix, apply the
same change locally, obtain commit/push approval, and push for an unassisted
run. The helper's `smoke ... --consent --unassisted` clears push options for
that one push. Promote only after the committed unassisted SHA passes.

### Choosing an IP or naming an endpoint

Clients may use a routable IPv4 or IPv6 address directly. Pass it as
`--client-host '<chosen IP>'`; the probe keeps it and needs no DNS change.
The installer adds the choice to the leaf certificate's IP SANs and shows it
in plan, Review and the install summary. IPv6 URL authorities use brackets,
for example `https://[2001:db8::10]:30443/mcp`. A matching CA pin alone does
not suffice: the leaf must cover the exact IP. Verification remains required.
The CA generator and its existing DNS name constraints are unchanged.

Alternatively choose a DNS name, for example `--client-host oberth.test`, and
pass the detected node or load-balancer address as `--endpoint-address` to
`scripts/install ports`. Show the returned `dns.hosts_line` verbatim for
`/etc/hosts` on each client, or `dns.dns_record` for the DNS administrator.
A hostname endpoint uses CNAME; an IP endpoint prints A/AAAA and a hosts line.
Never edit these automatically. Wait for the user's naming change, then rerun
with the chosen name and the same endpoint address. Continue when
`dns.resolves_to_endpoint` is true. Resolution is separate from TCP readiness.
Include the chosen DNS name and its certificate coverage in Review.

NodePort reachability by node IP depends on the network between client and
node. Guests behind NAT isolation cannot reach each other. Use port-forward,
or a DNS name or IP that is routable from the client; naming an unreachable
address does not create a route. A fresh server may not listen until install.

### No reachable node: LoadBalancer proposal

The port probe offers `load_balancer.install_flags` when no planned node
address answers its NodePorts. Missing node addresses and failed probes are
advisory generic signals, including on managed clusters; no provider detection
or new admission gate is used. Offer `--service-type LoadBalancer` with
`--client-https-port 443 --client-ssh-port 2222`, or port mapping/forwarding.
Use the operator's chosen routable DNS name or known IP in the same Review.
Include all flags in that Review and wait for "install" before any mutation.

For LoadBalancer, start the approved install in the background, retaining its
public output and process status. Then run the read-only address wait against
the saved plan and the reviewed namespace:

```sh
sh "$CLAUDE_PLUGIN_ROOT/scripts/install" ports --plan-file '<public plan JSON>' --client-host '<host or IP approved in Review>' --client-https-port '<approved HTTPS port>' --client-ssh-port '<approved SSH port>' --service-type LoadBalancer --wait-load-balancer --wait-timeout 2m --namespace '<approved namespace>'
```

The helper waits for Service `oberth` to exist and receive an ingress address,
with a bounded timeout (maximum 10m). It never creates a Service. For a chosen
DNS name, show the returned DNS remedy and wait for the user's DNS/hosts change
before re-probing with `--endpoint-address` set to the ingress address. An IP
choice is preserved directly. If the address differs from the reviewed choice,
review the new host and its certificate SAN before using it; discovery alone
does not update an already-issued certificate. Do not rerun the installer
while the original process is still running. On timeout inspect that Service
and its events, keep the install status and secrets file, then retry the wait
or review a forwarding remedy. Do not claim success until the original install
and subsequent verification pass. Standard LoadBalancer Service ports are 443
and 2222; NodePorts remain 30443 and 30022.

### Existing kind cluster without published ports

Before install starts, show `kind_remedy.config` in full when the selected
kind cluster has missing or unverified mappings; save it as
`oberth-kind.yaml` and show `kind_remedy.create_command` verbatim. It maps
container NodePorts 30022/30443 to the proposed SSH/HTTPS host ports (including
explicit overrides), bound to 127.0.0.1. Docker inspection recognizes only the
selected nodes' kind cluster. A reachable unrelated listener is insufficient
proof that kind has the required mappings.

The ready-to-use create command uses the new name `oberth-onboarding`; if
that exists, choose an unused name. Do not delete or recreate the selected
cluster automatically. The user can create a new cluster, select its context
and repeat the plan/probe, or keep this cluster and choose port-forward.
Print these remedies before the same install Review; kind is never refused
as an install target. These loopback mappings support clients on this Mac.

The printed command targets a new `oberth-kind.kubeconfig` and refuses an
existing file or symlink. Use that file and `kind-oberth-onboarding` explicitly
for the next plan/install; choose unused names instead of replacing files.

### Prerequisite hints on macOS

`scripts/install prerequisites` reports every missing kubectl/Helm tool and
prints the exact `brew install kubernetes-cli` and/or `brew install helm`
command on macOS, alongside the installation links. It then stops. The operator
runs dependency installation outside Oberth. Validation never invokes brew,
reads consent to install tools, or changes settings. Existing tools are not
reinstalled. Linux retains the installation links.

### Live NetworkPolicy enforcement check

Plan, install preflight and verification use a bounded traffic measurement on
the operator's cluster against Oberth's own server Pod. They do not identify CNI
brands or try to defend against a hostile server. A server and two differently
labelled sources per Ready, schedulable, untainted node run in an owned scratch
namespace. Both sources connect at baseline and after every policy removal.

Six selection rounds draw a fresh random bit per round, conditioned on including
both sources at least once. Each round requires two consecutive paired denial
samples per node. The selected source must time out, refuse or reset. While a
held response from the unselected source remains open, that same source must
also complete a fresh connection to the same server and port before and after
the selected attempt. This measures acceptance of new connections as well as
continuity of an established one. Any failed fresh connection is ambiguous.

A token-matched start chunk must arrive before the attempts, and the end chunk
must remain absent until after them. The probe then releases the held response
through its server Pod and requires successful completion. Missing overlap,
HTTP errors, changed Pod health and failed peers are unverified. Baseline and
recovery each require two consecutive successful samples from both sources.

The result is **measured enforcement** on the reported nodes for these six
rounds. Traffic measurements cannot prove causality: independent fault windows
can still coincide with every selection. Random repetition reduces some such
coincidences; it does not eliminate false passes. Read `enforced` as the wire
status for a completed measurement, not a proof of isolation. Evidence records
the actual selection order, per-round sample counts and node/namespace scope;
remedies describe observed failures. Elapsed measurement time includes owned
cleanup.

The public check's `evidence` states the scratch and pipeline namespaces,
client/control nodes and server node. The scratch namespace keeps only its
generated `oberth.ci/network-policy-probe` ownership label and restricted Pod
Security enforce/audit/warn labels with their versions set to latest. No
pipeline labels, including injection or privileged admission labels, are copied.
The owned policy selects the probe Pod label; no pipeline namespace label is
needed for that selection. Namespace-name/label-specific exclusions, other node-pair routes,
ingress enforcement and the installed policy's selectors/allow rules are not
covered by this TCP default-deny capability measurement. Read that scope with
the result; do not present it as proof of every installed isolation rule.
The pinned utility image supports arm64/amd64. An air-gapped installation needs
an accessible explicit `--utility-image` mirror; saved image pull secrets are
not copied into the scratch namespace.

A slow or non-converging policy is **unverified**, with the phase and observed
elapsed time, rather than proven absence. API, image, exec, server and route
failures are unverified too. Failed or disabled security checks refuse a fresh install or explicit
verification; repeat after repairing the reported condition. Required rights are
namespaces create/get/list/delete, Pods create/get, pods/exec get/create, and
NetworkPolicies create/delete, in addition to existing cluster reads. The probe
has a 120-second budget plus a separate 30-second cleanup budget after normal
return or cancellation. This is not a hard-kill cleanup guarantee: the next run
finds ownership-labelled leftovers and removes only an expired object with the
same listed UID, rechecking its expiry and binding deletion to UID/resource
version. Replacements, renewed lifetimes and missing/malformed expiry metadata
are preserved and reported as unverified. Recent or active probes stay until expiry. Foreign ownership
is reported and left untouched. Disclose these scratch writes before plan;
Review remains the approval for installing the release.

Capability is separate from configuration. Plan/install discover the existing
release and stored values before probing. Existing-release maintenance preserves
that choice and needs no new scratch-probe privileges: an unverified observation
remains unverified, shown as information, and does not block maintenance. Review
shows true, false or unspecified (preserved), with disabled/unknown warnings.
Fresh installs and explicit `install --verify` stay fail-closed. Verification
reads the effective installed boolean, so false is disabled and missing is
unverified, regardless of desired flags. Automatic post-maintenance JSON checks
retain informational probe results; they do not claim explicit security verification.
The dedicated upgrade path preserves stored values, prints the stored choice and
warning, and reports capability as unverified/not probed. A successful upgrade
is not NetworkPolicy evidence. Use `install --verify --json` for the explicit
bounded security check; rollback does not run this probe.

### Preserve the operator's kubeconfig

Selected kubeconfig files are read-only inputs. Native loaders disable
client-go migration and auth-provider persistence. Every kubectl/Helm child
(including backup, restore, unseal, upgrade and rollback) receives an owner-only
merged file for the child command, supplied through both its argument and
`KUBECONFIG` environment.
The copy preserves file precedence, the selected context and source-relative
CA, key, token-file and exec-command paths. A private child home prevents
client-go migration from creating an ambient default configuration; normal
Helm repository/cache/plugin directories remain available. Transient copies live in the owner-only
`oberth/kubeconfig-sessions` directory under the platform user cache. Each has
an owner-only marker with its creation time and a kernel lease inherited by the
child. Copies are removed after completion/cancellation; cleanup errors are reported.
SIGINT, SIGTERM, SIGHUP and SIGQUIT cancel the child and unwind cleanup.
After SIGKILL or a fatal runtime crash, a surviving child keeps its lease and
copy. After the last lease holder exits, the next ordinary CLI startup reclaims
recognized copies whose creation time is not in the future. Active copies,
unmarked/foreign objects and unverified lifetimes are never deleted by age alone.
Malformed markers or cleanup failures are reported for ownership inspection.
There is no wall-clock deletion guarantee if the child survives or the CLI never
starts again, and this is not a secure-erase guarantee. Source bytes, modes and sibling files
remain unchanged. Operator exec credential plugins retain their own environment
and may maintain their own caches; they are not sandboxed by this protection.

The add-repo script uses `oberth client kubectl` for the same boundary; keep the
verified local CLI in step with the plugin. Fetch, trust and connect do not
load or write a kubeconfig. Setup enumeration and probing use the same
migration-free native loader.

The CLI and setup wizard only target an existing operator-provided cluster and
kubeconfig. Oberth never creates or recreates a Kubernetes cluster.

Printed kind remedies use a new `oberth-kind.kubeconfig`, refuse an existing
file or symlink, and pass it explicitly to kind. After creation, use that file
and the corresponding context in plan/install. An ordinary operator-run
`kind create cluster` without a separate kubeconfig can switch current-context;
`kubectl config get-contexts` lists the contexts still retained in the selected
files. A changed current context does not establish that another context was
deleted. The rehearsal's original context loss remains unestablished; inspect
retained files and commands without attributing a cause or recreating guessed
credentials. Oberth never updates the operator's kubeconfig files.

Plugin connect requires an OpenSSL with `verify_ip` and `verify_hostname`.
It uses a capable `openssl`/`openssl3` on PATH or the standard Homebrew
`openssl@3` location on macOS. System LibreSSL without these checks is refused;
install OpenSSL outside Oberth (on macOS, `brew install openssl@3`) and retry.
The helper never installs tools or skips identity verification.
