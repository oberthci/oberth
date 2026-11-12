# Verified plugin bootstrap

`claude-plugin/scripts/fetch-oberth [version]` installs the newest release,
when no version is given, or a requested v-prefixed semantic version. It
requires curl or wget, OpenSSL, and sha256sum or shasum, plus the standard
POSIX shell utilities. Helm and kubectl remain user prerequisites for the
subsequent installation; the fetcher does not install either tool.

The script reads the newest version from
`https://releases.oberth.ci/oberth/latest/VERSION`, then downloads only
immutable objects under `https://releases.oberth.ci/oberth/<version>/`.
It authenticates `SHA256SUMS` using `SHA256SUMS.sig` and the public key
shipped in `claude-plugin/pins/release-cosign.pub`. It does not accept a key
from the network or an environment variable. The key file's SHA-256 pin is
checked before any download. CI's `TestPinnedKeyIsTheReleasePipelinePin`
checks the key bytes and pins in the plugin, release pipeline, and CLI;
this test belongs to the release race-test rest group.

The manifest must contain exactly the four supported CLI binaries and
`oberth-<version without v>.tgz`, without duplicate or unexpected paths.
The fetcher hashes only the selected binary, using its own filename. It
never executes a downloaded file during verification. After verifying, it
stages the binary on the destination filesystem, installs it with mode
0755 at `~/.local/bin/oberth-<version>` (including the leading `v`), and
atomically replaces the `~/.local/bin/oberth` symlink. A concurrent fetch
is refused. A failed download or verification leaves the existing binary
and symlink in place. A killed process can leave `.oberth-fetch-lock` in
that directory; remove the empty directory only after checking that no
fetch is running, then retry.

The plugin commands' `allowed-tools` entries permit only their own
scripts. The install wrapper checks Helm and kubectl, calls the verified
`~/.local/bin/oberth` directly for plan, install and verify, and refuses an
install without an explicit nonempty `--secrets-file`. Other shell operations in the guided install still use Claude
Code's normal permission checks. The Review confirmation remains the
explicit gate for installing into the context the user chose.

## Release verification

The release pipeline packages the chart before signing `SHA256SUMS`.
It takes the raw DER signature from the authenticated Sigstore bundle,
checks the bundle's SHA-256 digest, and verifies the DER signature using
OpenSSL. Before publication and finalization it also compares the binary
and chart digests with `release.json`. Both signatures authenticate the
same checksum file; no private key conversion or export is needed.

For manual verification, obtain `release.pub` from the trusted source
checkout's `.oberth/pins/release-cosign.pub`, and download `SHA256SUMS`,
`SHA256SUMS.sig`, all four binaries and the chart from the same versioned
release directory:

```sh
openssl dgst -sha256 -verify release.pub -signature SHA256SUMS.sig SHA256SUMS
sha256sum -c SHA256SUMS
```

Never substitute a public key downloaded beside the artifacts for the
trusted source pin.

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

### Optional Homebrew prerequisites on macOS

`scripts/install prerequisites` reports missing kubectl/Helm and, on macOS
with Homebrew already present, prints the exact `brew install kubectl` and/or
`brew install helm` command and offers to run it. It performs no installation.
After the user's explicit yes, `scripts/install prerequisites --install-missing`
prompts separately for each missing tool. Only the literal answer `yes` on
stdin runs that exact command. Decline/EOF installs nothing. A failed brew
command or a tool still absent from PATH stops the flow. Linux and Macs
without Homebrew retain the installation links. Homebrew itself is never
installed automatically; existing tools are never reinstalled.

### Live NetworkPolicy enforcement check

Plan, install preflight and verification use a bounded live egress probe, not
controller names or CNI brands. A server and one client/control pair per Ready,
schedulable, untainted node run in an owned scratch namespace. The control Pod
shares its client's node and stays outside the egress-deny selector. Receiver
ingress stays open. Each denial observation checks a working path to the same
server before, throughout, and after the tested client request. HTTP errors, refused
listeners, changed Pod health or failed controls are unverified. Only timeouts
without an HTTP response are denial candidates; three observations and two
separate deny/recover transitions are required.

The public check's `evidence` states the scratch and pipeline namespaces,
client/control nodes and server node. Existing pipeline namespace labels are
copied except Kubernetes' immutable namespace-name label; the probe adds its
ownership label. Namespace-name-specific exclusions, other node-pair routes,
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
has a 90-second budget plus a separate 30-second cleanup budget after normal
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
