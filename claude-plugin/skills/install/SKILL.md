---
name: install
description: Install a new Oberth server on the Kubernetes cluster the user's KUBECONFIG names, without any secret entering this session. Use when the user runs /oberth:install, asks to install or set up Oberth, or the session-start note says no Oberth server is configured and the user wants one.
argument-hint: "[kubeconfig context]"
allowed-tools: Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/fetch-oberth" *), Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" *)
---

# Install Oberth

Leave `url` and `ca_pin` empty before a server exists; continue with
`/oberth:install`, which fills the installed connection settings. Some Claude
Code versions still print "2 userConfig options not yet set" for optional
empty values. That notice does not block installation; do not invent a URL or
CA pin or run configure just to silence it.

Oberth installs with its own CLI onto a cluster the user already has. This
guide drives `oberth install` non-interactively. The install creates
credentials that are shown once (the OpenBao root token and unseal key, the
first admin's client certificate (public)). They go to a secrets file, never into this
session.

Arguments given to the command: `$ARGUMENTS`

## Steps

1. **Prerequisites.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" prerequisites`.
   Missing Helm or kubectl prints installation links and, on macOS, the exact
   Homebrew commands. Show those commands and stop. The operator installs the
   dependencies outside Oberth; neither this skill nor the wrapper runs brew,
   even after a yes. Recheck prerequisites only after the operator finishes.
2. **The CLI.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/fetch-oberth"` (or pass an
   explicit release version). It uses the plugin's pinned release key, OpenSSL
   and SHA-256 to verify before installing into `~/.local/bin`. It needs no
   sudo, cosign, jq or Python. A verification failure stops the flow.
3. **The cluster.** Explain that plan only reads the cluster. The bounded
   NetworkPolicy probe below runs first at install, after approval.
   Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" plan` with the
   chosen `--context` and answers file when available. Show its kubeconfig
   files, API URL, CA pin, node facts and remedies. Any chosen context is a valid
   target; never add a local/remote gate. With multiple contexts, ask the user.
   Never pick one yourself.
4. **Ask, in plain words, four things:**
   1. Real use or a quick evaluation? Real use is `--install-secretstore`
      (OpenBao in production mode; the user keeps the unseal key). An
      evaluation is `--install-secretstore-dev` (in memory, lost on restart).
   2. Where does green code go? For GitHub, Codeberg or GitLab:
      `--forge github|codeberg|gitlab --organization <org>`. For any other
      forge: `--upstream-url ssh://git@<host>[:<port>]/<org>`, together with
      `--expected-host-fingerprint SHA256:...`, the forge's SSH host-key
      fingerprint as its admin or documentation states it. Never take a
      fingerprint from a scan of the forge yourself: that is trust on first
      use.
   3. How do clients reach the server? On this machine: `--client-host
      localhost` with a local mapping or forward. From other machines: a routable
      DNS name or IP; an IP choice must be covered by an IP SAN. Reachability
      depends on the network between client and node, including NAT isolation. When the
      NodePorts are forwarded to other ports, the next step proposes `--client-ssh-port` and
      `--client-https-port`; the user does not need to discover these numbers.
   4. Turn on assisted continuation (`--assist`), so a failed step of a run
      the agent pushed can be held for it to fix in place? Off by default.
5. **Check client reachability before install.** Save the public JSON from
   `helper plan` (the `scripts/install plan` command above) to a new local
   plan file, rerunning it with the chosen address/options if needed. Run:

   ```sh
   sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" ports --plan-file '<public plan JSON>' --client-host '<chosen hostname or IP>'
   ```

   Pass explicit client port overrides and `--service-type` too when selected.
   The helper uses this plan's node identities to read the matching local
   Docker cluster's published ports, including k3d serverlb/node bindings.
   Docker is optional. Propose the returned `install_flags` in the install
   review, including forwarded ports automatically; never guess a global
   `oberth` SSH alias. Explicit ports win. A remote Docker daemon's ports
   and another cluster's bindings are not this host's mappings.
   Show `kind_remedy` in full before install when present (see below).
   When `load_balancer` is returned, offer its flags in the same Review and
   follow the bounded address wait below after the approved install starts.
   For an IP or load-balancer address, offer the direct IP or optional DNS
   naming path below; preserve the user's choice.
   `needs_action` can mean a DNS mismatch even when both TCP checks succeeded.
   Explain the returned DNS remedy and individual TCP observations separately.
   A failed TCP check means that port did not answer; it does not establish a
   missing route. If access still fails after install, offer a routable name/IP
   or port-forward and repeat the probe with the resulting client ports. The probe is advisory: a fresh
   server may not listen yet, and TCP or a mapping proves neither readiness
   nor TLS trust. Every cluster type uses the same install Review; this is
   not a local/remote or cluster-type gate.
6. **Local identity.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" keygen`.
   The private client key stays under `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/` with mode 0600.
   The current identity is `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/current/`.
   In executable examples use `$HOME` for the fallback, as below.
   Reuse the user's SSH public key or generate an Ed25519 SSH key locally;
   never upload either private key. Set `--admin-ssh-public-key` to its public
   file and `--admin-csr "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/oberth/current/client.csr"`. For an SSH target,
   copy only the CSR and SSH public key and return the issued certificate over
   that authenticated SSH connection. Never set `--admin-bearer` by default.

7. **Review.** Explain that the bounded enforcement check runs first after
   approval, creating a scratch namespace, Pods and deny policies and cleaning
   them up. A fresh install refuses absent or unverified enforcement. Show the
   complete command and wait for the user to say "install".

   ```sh
   sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" run --yes --json --context '<context>' --install-secretstore \
     --secrets-file "$HOME/oberth-install-secrets.txt" \
     --admin-ssh-public-key '<local SSH public key file>' \
     --admin-csr "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/oberth/current/client.csr" --client-access none \
     --forge github --organization '<org>' --client-host localhost
   ```

   Save the public install JSON result outside the repository: add-repo uses
   its exact `server_url`, `git_url` and `ca_sha256` to bind the selected server.
   Include the probed client port flags, the Review upstream name (change it
   with `--upstream-name` if requested), and every required port-forward or
   DNS/hosts step in this reviewed command and explanation. The user performs
   routing steps; never start a forward or edit hosts yourself.

   For a fresh install the secrets file must be a new file outside any git
   worktree. For recovery, give only its path to `--resume-secrets-file` and
   choose a new `--secrets-file` output path. Reusing an existing private
   `--secrets-file` as production recovery input keeps it intact and reserves a
   new sibling output file for fresh credentials; keep both when one is saved.
8. **Run it** exactly as reviewed. It takes several minutes: run it in the
   background or with the longest command timeout, and never interrupt it.
   The output names the secrets file and its SHA-256, and nothing secret.
   Tell the user to move that file off this machine (a password manager, an
   offline copy) and to delete it here.

   When the install stops part-way, the secrets file it names is kept and
   may hold the only copy of the OpenBao root token and unseal key. Leave it
   where it is, tell the user, and resume with `--resume-secrets-file` naming
   that input and a new `--secrets-file` output name. Keep all reviewed answers.
   Keep the original `--admin-csr`, `--admin-ssh-public-key` and identity flags.
   The installer itself retains a valid client-auth certificate matching the
   original CSR key and admin identity, without repeating enrollment or minting
   credentials. A mismatched, expired, wrong-purpose or malformed certificate
   stops resume; never delete or replace the certificate to force enrollment.
   Report the error and stop if the original matching inputs are unavailable.
   When `client_bootstrap_endpoint` fails, show its URL, observed error and
   remedy: installed releases and credentials remain, and the user must make
   the endpoint reachable before resuming. Run documented verify to check
   progress after the user fixes routing; an already-issued client certificate
   is kept. Use the printed verification command to resume without reissuing it.
   Rerun installation only when verification shows unfinished onboarding; do
   not repair it through in-Pod administration.
9. **Add this key at the returned link with write access.** Parse the JSON even
   on exit code 3 (installed, action needed). For `upstream_key_pending`, show
   `upstream.deploy_public_key`, `upstream.fingerprint`, and
   `upstream.forge_settings_url` first. The registered upstream is pending;
   Oberth is Ready and MCP/mTLS can connect before the forge accepts the key.
   - GitHub deploy keys belong to a **specific repository**, not an organization.
     Use `/oberth:add-repo` and that repository's Settings → Deploy keys, with
     write access. With no repository yet, the user can create one later.
   - For organization-wide use, the returned GitHub settings link is the SSH-key
     page: sign in as a **machine user** with write access to the intended
     repositories and add the key there. Do not add it to the operator's personal
     account by default. The same choice applies to other supported forges.
   - After the key is authorized, run the documented installer verify command
     with the selected context and reviewed endpoint flags. `upstream.next_command`
     may describe in-Pod administration for an operator: never execute it from
     this skill. If verification still requires onboarding, use the documented
     installer resume path or stop and ask the user. Use the JSON fields, never
     discarded text output or a guessed upstream name.
   - Exit code 0 means all checks passed; exit code 1 is a real failure. Resolve
     failing checks before declaring success. Do not reinstall merely because
     the deploy key is pending.
   - When the output says the client files were kept because they differ,
     this machine is set up for another Oberth server. Ask the user before
     rerunning with `--replace-client-files`; never add it yourself.
   - Use the install result's URL and CA pin with `/oberth:connect` (the
     pin came from the authenticated installer). Run the plugin's `trust
     credentials` command with the issued certificate and the local key, then
     `trust configure '<https URL>/mcp'`. It adds a user MCP entry only after
     configuration and has no Authorization header in certificate mode.
     Existing settings and env values are kept. Restart Claude Code once,
     confirm `/status` shows the mTLS client certificate row, and call
     `system_status` to prove authentication. Never read the secrets file.
   - The first admin is the user's locally generated identity. No bearer
     token is generated unless the user explicitly selects `--admin-bearer`.

## Rules

- Never print, read, grep, copy or upload the secrets file, and never pass a
  token or a key on a command line. Moving the token into a secret store goes
  through the command the install printed, which reads the file without
  showing it.
- Never delete, move, truncate or overwrite a secrets file, from this run or
  an earlier one. Use the old file only as installer recovery input and choose
  a new output name instead of removing the old file.
- A `--dry-run` prints a plan and installs nothing. After the approved
  install, run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" verify --context
  '<context>'` and explain every failing check and remedy.
- Never disable TLS verification, and never trust the server's CA any other
  way than `/oberth:connect` with the pin.

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
for the next plan/install; choose unused names if either already exists.

### Live NetworkPolicy enforcement check

Install preflight (after approval) and verification use a bounded traffic measurement on
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
HTTP errors, changed Pod health/UID and failed peers are unverified. Pod UIDs
are captured from creation and checked during readiness, around every exec and
at completed traffic stages. A same-name replacement cannot supply credited
measurement results. Exec explicitly names the probe container; kubectl exec
has no atomic UID precondition, so a replacement can race the command but its
result is rejected when the UID observations differ. Baseline and
recovery each require two consecutive successful samples from both sources.

The result is **measured enforcement** on the reported nodes for these six
rounds. Traffic measurements cannot prove causality: independent fault windows
can still coincide with every selection. Random repetition reduces some such
coincidences; it does not eliminate false passes. Read `enforced` as the wire
status for a completed measurement, not a proof of isolation. Evidence records
the actual selection order, per-round sample counts and node/namespace scope;
remedies describe observed failures. Elapsed measurement time includes owned
cleanup. Every round deletes its policy with the UID returned by creation; a
same-name replacement makes deletion fail and the result stays unverified.

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
is reported and left untouched. Plan makes no cluster mutations and reports
enforcement as unverified. Disclose these scratch writes in Review; the
enforcement check is the first cluster mutation after approval, before release
changes. No measurement receipt is written or reused.

Capability is separate from configuration. Plan/install discover the existing
release and stored values before applying. Plan only reads those values; install
then runs a fresh probe before release changes. Existing-release maintenance preserves
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

## Installer failure guardrail

When the installer fails or gives no clear reason, stay on the installer path:
read the public result JSON and its checks, endpoint, observed error and remedies.
Run only the documented installer verify or resume command as printed, preserving
the reviewed context and answers. If the result has no actionable reason, stop
and ask the user; never invent a workaround outside the documented commands.
Never run administrative commands inside the server Pod (including upstream add
or enrollment), directly or through another helper, to work around an installer
failure. Do not switch to another skill to bypass this rule.
Never read, pipe or export the secret store's root token or any value from the
secrets file. Give only file paths to the documented installer or unseal command;
the executable reads credentials privately. Never ask the user to paste a token
or export BAO_TOKEN for the agent.
Report only verified facts. An installer exit code or an incomplete result does
not establish whether anything was pushed to the forge. Say what was observed,
what remains deployed and what is unknown; never claim that nothing was pushed
without checking evidence that establishes that fact.
