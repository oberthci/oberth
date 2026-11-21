# Install and maintain

Use the [first-build commands](../README.md#first-green-build) on Kubernetes 1.30 or newer.

## Obtain the CLI

Run `sh <skill-directory>/fetch-oberth` from the [trusted skill](../agent/skills/oberth/SKILL.md).
The [fetcher](../agent/skills/oberth/fetch-oberth) and CLI use
`https://releases.oberth.ci/oberth`, the [canonical address](../internal/releaseverify/origin.go).
The fetcher verifies the signed manifest and binary digest before replacing a binary; missing
releases or signing keys stop it. Use the printed absolute path, never a checkout executable.

## Review and install

`oberth install --context <context>` asks for missing answers and approval on a terminal, paging
a tall review (Enter). Defaults: `https://<API host>:30443`, SSH 30022; clients must reach them.
Kubeconfig is unchanged. For unattended installation, review before applying:
```sh
oberth install --context <context> --dry-run
oberth install --context <context> --yes
```
`--yes` skips confirmation; keep choices identical between review and apply, including
`--url <https-url> --ssh-port <port>` for mapped ports. Baseline Pod defaults and the link-local
egress filter default on ([boundary flags](security.md#what-this-does-not-protect)). Compact review
names context, cluster UID, namespaces, version, endpoints, run cap, store, policies and files.
Images are in `oberth install --dry-run --output json` and `oberth install --dry-run --detail`.
Cluster UID prevents cross-cluster recovery; signatures and digests are checked first.
The chart includes the server, Argo and secret store. Keep the profile path, SSH fingerprint,
deploy key and CA pin, which authenticates HTTPS. Check the endpoint with `oberth system status`.

Follow [repository registration](../README.md#first-green-build) to add the deploy key and verify
the forge fingerprint. The server owns the forge pin. `--known-hosts <known-hosts-file>` separately
pins its SSH endpoint. Use the printed `SSH trust` path (default `~/.ssh/known_hosts`).
Exit 3 means the forge rejected the deploy key: add it, then retry. This binds remote `oberth`.
`--upstream <name>` selects a repository identity, preserving existing access and trust settings;
otherwise registration reuses a matching upstream or derives its name from the host. Use the printed
repository path for step accounts and release subjects. After joining, follow
[checkout binding](agents.md#install-invite-and-join) to select an already registered repository.

## A second installation in the same cluster

Choose another namespace and two unused node ports. The pipeline namespace defaults to
`<namespace>-runs`; `--pipeline-namespace` selects another. For example:
```sh
oberth install --context <context> --namespace second \
  --https-node-port 31443 --ssh-node-port 31022 --dry-run
oberth install --context <context> --namespace second \
  --https-node-port 31443 --ssh-node-port 31022 --yes
```
Endpoints default to the API host and chosen ports; `--url`/`--ssh-port` override public mappings.
Review shows each public port beside its node port. If a port is busy, an interactive install asks
for another; unattended commands refuse and name the flag to change. The profile defaults to
`<context>-<namespace>`; `--profile` selects another. Reruns, upgrades and rollback require its
original administrator key. A missing or different key causes a refusal. Saved namespaces and
ports are reused. If no release revision exists, explicit answers replace the saved record and the
review shows the changes. Pending revisions must match saved answers. Keep the profile while its
administrator Secret remains. Hand-made namespaces stay unmarked and excluded from removal;
`default` and `kube-*` are forbidden. Compatible foreign-owned definitions stay unchanged; review
names their owner. Required schema fields and served/storage versions must match our pinned
controller. Controller and workflows use the server namespace as Argo instance ID, so default Argo
controllers skip them. Our controller watches only its pipeline namespace. Cluster-wide permission
and policy names include custom namespaces. Rollback preserves shared workflow definitions.

Removing the Argo release that owns the workflow definitions keeps them by resource policy, but
every run object depends on those definitions. Upgrading that release with its chart defaults
replaces the definitions' contents. The definitions' owner must upgrade only to a version Oberth's
release notes name as compatible. Install and upgrade refuse workflow definitions that would drop
fields this version submits; after their owner upgrades them, run `oberth upgrade --dry-run` to
check compatibility again.

## Keep the recovery files

The profile is `~/.config/oberth/<profile>/`; `installation.json` records paths. Copy it, archives,
`backup.agekey` and `recovery` to private off-machine storage.
**The five installation Secrets (CA key, forge deploy key, SSH host key, store TLS, administrator)
are in no backup**; save them with the [Back up command](#back-up). Only a storage-level copy of
the data volume can recover the database today. `recovery` unseals a store; it cannot restore one.
Install reuses the administrator key; if missing, it refuses. Select with `OBERTH_PROFILE` or
`~/.config/oberth/current`; agent joining preserves that selection.

## Agent plugins

Once the repository is public, Claude Code installs the skill and run box with
`/plugin marketplace add oberthci/oberth` and `/plugin install oberth@oberthci`. From join's
`agent tools: stdio MCP server "oberth" = ...` line, copy the absolute executable path (possibly
quoted) and uplink profile after `--profile`:
```sh
claude plugin configure oberth@oberthci --values-stdin <<'JSON'
{"executable":"/absolute/verified/path/from-join","profile":"uplink-profile-from-join"}
JSON
```
Set the agent's uplink profile here or in `OBERTH_PROFILE`; the option takes precedence. No
profile hides the box. Use the verified executable, never a checkout file. The box shows up to
three runs, progress and failed steps; refresh is every 15 seconds when active/watched, 60 when
idle. `/oberth-runs [--limit=N]` shows the table; `/oberth-watch <id-or-prefix>` watches until the
completion toast. The `runs` and `watch` tools provide the same actions. Codex installs the
existing skill through `agent/.codex-plugin/plugin.json` with
`codex plugin marketplace add oberthci/oberth`, then `codex plugin add oberth@oberthci`. Register
join's exact bridge line as in [agent connection](agents.md#connect-the-tools); this package has
no above-prompt view for Codex.

## Upgrade

When the review shows a schema change, take a storage-level copy of the data volume before applying.
```sh
oberth upgrade --dry-run
oberth upgrade --yes
```
Upgrades require signed releases; local charts use `oberth install --chart <directory>`. Add
`--version <version>` to both commands to select a version. Offline `--release-dir <directory>`
accepts signed release files or a parent of version directories such as `v0.1.0` and `v0.2.0`.
The version directory takes precedence over the flat one; invalid version directories refuse.
Recorded same-schema repair needs both releases; select their parent. Review preserves settings and
shows changed defaults, schema and revision; JSON has images. Direct Helm changes appear in one
review line; install and upgrade replace them. Rollback and repair refuse those changes and point to
install or upgrade. Review failed revisions. Finish unlabelled Workflows before upgrading (not
adopted). Runs continue through restart; one submitted while the workflow controller restarts can
fail at step start: `oberth run rerun <id>`. Upgrade backs up the database in the profile by version
and UTC time; copy off-machine. Success keeps the three newest copies, failure its recovery files.
The record binds cluster UID, revisions, versions and backup digest; `--no-backup` opts out.
Verified completion and rollback audit versions and revision. An unconfirmed receipt doesn't undo
success: use the printed audit command without repeating the upgrade. Readiness waits up to ten
minutes and ends when ready. After a timeout, wait for `oberth system status` to be ready, then
run `oberth upgrade`, which completes a ready target's record unchanged, or `oberth rollback --yes`.
After a schema change, if it stays down, recovery is manual: keep the encrypted pre-upgrade archive,
`backup.agekey` and `rollback.json` in `~/.config/oberth/<profile>/` and copy them off-machine.

## Roll back

Review the saved record with `oberth rollback --dry-run`, then run `oberth rollback --yes` with its
original profile. A different schema, cluster UID or unexpected revision refuses before writes.
Repeating resumes rollback; without the record, inspect the deployed revision. Offline, use
`--release-dir <directory>` with signed source-version files; another version refuses.

## Back up

`oberth backup --out <new-archive-file>` saves an age-encrypted database; no restore command ships.
**Only a storage-level copy of the data volume can recover the database today.**
Backup never overwrites a file; use local storage with hard-link support, then copy off-machine.
`--dry-run` previews the destination. Database input defaults to 1 GiB, maximum 4 GiB;
archive framing has its own bound, separate from encrypted-file size.
For automation, `--output json` publication warnings exit 0: the optional `warning` has `code`,
`reason`, `remedy`, optional `id` (subtype), `context` (recovery facts), `detail` (diagnostics).
Warnings also use standard error; clean and dry-run results omit `warning`. Keep a published
archive and fix its storage problem. **The five installation Secrets are in no backup:** CA key,
forge deploy key, SSH host key, store TLS and administrator; save all five to private storage:
```sh
(umask 077; kubectl --context "$context" -n "$namespace" get secret \
  oberth-ca oberth-forge-key oberth-ssh-host oberth-store-tls oberth-admin -o yaml \
  > /private/storage/oberth-secrets.yaml)
```
**The bundled store's contents are not in any backup**; no store snapshot procedure or operator
token ships; use `oberth secret put` to put secrets again from secure originals after loss.
Keep `oberth access list --repo <repo> --kind store_path --json` output with the backup: it lists
approved paths, workflows and steps; no values or proof of stored values. Keep `recovery` private.

## Unseal after restart

After restart, run `oberth unseal` on the installation machine with its selected profile and saved
`recovery` file; never print it. Install configures the bundled store's static rules and
installation audience. For external stores, run `oberth secretstore check` and `oberth secretstore
setup` in an operator Pod with CLI and store access. Supply `--config` (server configuration and one
store audience), `--ca-file` (store TLS CA) and `--token-file` (private OpenBao operator token).
Check needs read access to auth methods, policies and roles; setup needs configure access. The
administrator profile lacks these files. A projected Kubernetes login token is not an OpenBao
operator token. The external operator must supply the token and run setup before use; bundled
install supplies none.

## Remove an installation

There's no uninstall command: remove with Helm directly; install, upgrade and roll back with Oberth.
Set `context` and `namespace` for this installation. Back up the database, keep secret names and
original values separately, and copy recovery material and archives to private storage.
```sh
helm --kube-context "$context" uninstall oberth -n "$namespace" --wait
```
Helm removes the release's server, controller, store, permissions and policies, and its pipeline
namespace only if the installation created it. Pre-existing namespaces stay. These objects remain
in the server namespace; Helm never owned the five Secrets:

| Object | What remains |
| --- | --- |
| PVC `oberth-data` | Server database; Helm's resource policy keeps it. |
| PVC `data-oberth-store-0` | Bundled store data; its StatefulSet leaves this claim behind. |
| Secret `oberth-admin` | Administrator public keys used to authorize access. |
| Secret `oberth-ca` | Installation CA certificate and private signing key. |
| Secret `oberth-forge-key` | Forge deploy private key and public key. |
| Secret `oberth-ssh-host` | Server SSH host private key and public key. |
| Secret `oberth-store-tls` | Store TLS certificate and private key. |

Save data and keys; choose ONE cleanup. Keep volumes for data; storage class controls reclamation.
Shared workflow definitions stay, including ours; deleting them deletes every installation's runs.

### Installation-created namespace

Install marks only namespaces it creates with Helm release-name and release-namespace annotations.
First confirm it contains no other workloads. Deletion removes everything; this command requires
a matching creation mark and no remaining Helm release Secrets:
```sh
mark='{.metadata.annotations.meta\.helm\.sh/release-name}{"/"}'
mark="$mark"'{.metadata.annotations.meta\.helm\.sh/release-namespace}'
owner=$(kubectl --context "$context" get namespace "$namespace" -o "jsonpath=$mark")
if [ "$owner" = "oberth/$namespace" ] &&
  releases=$(kubectl --context "$context" -n "$namespace" get secrets -l owner=helm -o name) &&
  [ -z "$releases" ]; then
  kubectl --context "$context" delete namespace "$namespace"
fi
```
If either check fails, keep the namespace and use the retained-namespace commands.

### Retained namespace

Keep a pre-existing shared namespace. In a hand-made namespace, or one now shared with other
workloads, remove only this installation's two claims and five Secrets by name:
```sh
kubectl --context "$context" -n "$namespace" delete pvc data-oberth-store-0 oberth-data
kubectl --context "$context" -n "$namespace" delete secret \
  oberth-admin oberth-ca oberth-forge-key oberth-ssh-host oberth-store-tls
```

### Retained pipeline namespace

A pre-existing pipeline namespace keeps step ServiceAccounts and run objects after Helm removal.
Set `pipeline_namespace` to the reviewed name; remove only by these labels, never by name patterns:
```sh
instance="workflows.argoproj.io/controller-instanceid=$namespace"
kubectl --context "$context" -n "$pipeline_namespace" delete cronworkflows.argoproj.io \
  -l "app.kubernetes.io/managed-by=oberth,$instance"
kubectl --context "$context" -n "$pipeline_namespace" delete workflows.argoproj.io \
  -l "oberth.ci/engine=argo,$instance"
kubectl --context "$context" -n "$pipeline_namespace" delete serviceaccounts \
  -l app.kubernetes.io/managed-by=oberth
```
The selector checks the server namespace's controller ID; other runs stay. Deleting Workflows
removes dependent Pods; keep the pipeline namespace itself.

### Local profile

After preserving its recovery material and archives, remove the local profile using its path:
```sh
rm -r -- /absolute/path/to/installation-profile
```
This removes its credentials, recovery material, records and backups. Use `OBERTH_PROFILE` to select
a remaining installation, or `oberth install --profile <new-profile>` for a fresh install.

## Command counts

Counts include the [first green build](../README.md#first-green-build), refused registration
and verified retry; a required environment variable adds one flag.

| First green path | Commands | Flags |
| --- | --- | --- |
| Terminal, default ports: install with `--context` | 8 | 5 |
| Unattended, mapped ports: apply only, with `--yes --url --ssh-port` | 8 | 8 |
| Agent, mapped ports: address and SSH port on both commands | 9 | 12 |

Omit `--known-hosts` if the default file has the verified SSH key; subtract two flags.
The forge pin is server-owned. A dry run adds one command and its flags, included in the agent row.
The three release commands assume no refusals; missing grants add approval commands and another
release attempt. `secret put` takes three flags.
