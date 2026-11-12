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

After selecting the context and advertised DNS name, save the **public**
`install --plan --json` result to a new file. This contains cluster identity
and checks, never installer secrets. Before the install Review, run:

```sh
sh "$CLAUDE_PLUGIN_ROOT/scripts/install" ports --plan-file '<public plan JSON>' --client-host localhost
# Equivalent CLI:
oberth client ports --plan-file '<public plan JSON>' --client-host localhost
```

Clients use DNS names (`localhost`, the host name or `<host>.local`), never IP
addresses. Keep the CA's DNS-only name constraints; IP constraints are deferred
until Bun releases its fix, as described in [client certificates](client-certificates.md).

The result proposes `install_flags` with the HTTPS and SSH host ports. It
uses exact node identities from that plan to find a matching local Docker
cluster's published TCP ports, including k3d's serverlb and direct node
mappings. For example, mappings from 30443 to 32443 and 30022 to 32022 propose
`--client-https-port 32443 --client-ssh-port 32022`. Explicit client port flags
override discovery. Pass the selected `--service-type` when it is not NodePort.

Docker is optional. Other clusters get the same TCP probe of their chosen
address; no cluster type controls install admission. Remote Docker daemon
bindings, unrelated clusters, other host interfaces and ambiguous mappings
are not silently selected. When `needs_action` is true, explain:
"NodePorts are not reachable from this host; map them or use port-forward."
Propose that network setup and repeat with its host ports. The helper never
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

### Naming an IP or load-balancer endpoint

When the only endpoint is an IP or load-balancer address, pass it as
`--endpoint-address '<detected address>'` to `scripts/install ports`. If no
name was chosen, propose `--client-host oberth.test` (the user may choose their
own DNS name). Show the returned `dns.hosts_line` verbatim for `/etc/hosts` on
each client, or `dns.dns_record` for the DNS administrator. A hostname endpoint
uses a CNAME record; IP endpoints print an A/AAAA record and a hosts line.
Never edit these automatically. Wait for the user to configure the name, then
rerun with the chosen `--client-host` and the same `--endpoint-address`.
Continue when `dns.resolves_to_endpoint` is true. This checks name resolution
separately from TCP: a fresh service need not listen yet. Keep DNS-only CA
constraints and include the chosen name in the install Review.

### No reachable node: LoadBalancer proposal

The port probe offers `load_balancer.install_flags` when no planned node
address answers its NodePorts. Missing node addresses and failed probes are
advisory generic signals, including on managed clusters; no provider detection
or new admission gate is used. Offer `--service-type LoadBalancer` with
`--client-https-port 443 --client-ssh-port 2222`, or port mapping/forwarding.
Use the operator's chosen DNS name (propose `oberth.test`) in the same Review.
Include all flags in that Review and wait for "install" before any mutation.

For LoadBalancer, start the approved install in the background, retaining its
public output and process status. Then run the read-only address wait against
the saved plan and the reviewed namespace:

```sh
sh "$CLAUDE_PLUGIN_ROOT/scripts/install" ports --plan-file '<public plan JSON>' --client-host '<hostname approved in Review>' --client-https-port '<approved HTTPS port>' --client-ssh-port '<approved SSH port>' --service-type LoadBalancer --wait-load-balancer --wait-timeout 2m --namespace '<approved namespace>'
```

The helper waits for Service `oberth` to exist and receive an ingress address,
with a bounded timeout (maximum 10m). It never creates a Service. Show the
returned DNS remedy, wait for the user's DNS/hosts change, and re-probe with
`--endpoint-address` set to that ingress address. Do not rerun the installer
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

The CLI and setup wizard only target an existing operator-provided cluster and
kubeconfig. Oberth never creates or recreates a Kubernetes cluster.
