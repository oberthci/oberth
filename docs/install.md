# Install and maintain
Use the [first-build commands](../README.md#first-green-build) on Kubernetes 1.30 or newer.
## Obtain the CLI
Run `sh <skill-directory>/fetch-oberth` from the [trusted skill](../agent/skills/oberth/SKILL.md).
The [fetcher](../agent/skills/oberth/fetch-oberth) and CLI read GitHub releases, follow only GitHub
asset redirects and verify signed manifests/digests against the pinned key before replacement.
Release base: `https://github.com/oberthci/oberth/releases/download`. Missing releases/keys refuse.
Use the printed absolute path, never a checkout executable.
## Review and install
`oberth install --context <context>` asks for missing answers and approval on a terminal, paging a
tall review (Enter). Defaults: `https://<API host>:30443`, SSH 30022; clients must reach them.
Kubeconfig is unchanged. For unattended installation, review before applying:
```sh
oberth install --context <context> --dry-run
oberth install --context <context> --yes
```
`--yes` skips confirmation; keep choices identical between review and apply, including
`--url <https-url> --ssh-port <port>` for mapped ports. Pod defaults and run egress default on
([boundary flags](security.md#what-this-does-not-protect)). Compact review names context, cluster
UID, namespaces, version, endpoints, run cap, store, policies and files. Images are in
`oberth install --dry-run --output json` and `oberth install --dry-run --detail`. Cluster UID
prevents cross-cluster recovery; signatures and digests are checked first. The chart includes the
server, Argo and secret store. Keep the profile path, SSH fingerprint, deploy key and CA pin, which
authenticates HTTPS. Check the endpoint with `oberth system status`.
Follow [repository registration](../README.md#first-green-build) to add the deploy key and verify
the forge fingerprint. Use the forge's SSH URL with its own user and path, such as
`ssh://<user>@<forge>/<path>/<repo>.git`; `<user>@<forge>:<path>` is in its home, `git@` from root.
The server owns the forge pin; the profile's `SSH trust` file pins the Oberth server's SSH keys
(`--known-hosts` replaces it). Exit 3: the deploy key was rejected; add it, retry. Binds `oberth`.
`--upstream <name>` selects a repository identity, keeping its settings; otherwise it reuses a
matching upstream or derives a name from the host. Use the printed repository path for step accounts
and release subjects. After joining, see [checkout binding](agents.md#install-invite-and-join).
## A second installation in the same cluster
Choose another namespace and two unused node ports. The pipeline namespace defaults to
`<namespace>-runs`; `--pipeline-namespace` selects another. For example:
```sh
oberth install --context <context> --namespace second \
  --https-node-port 31443 --ssh-node-port 31022 --dry-run
oberth install --context <context> --namespace second \
  --https-node-port 31443 --ssh-node-port 31022 --yes
```
Endpoints use the API host and chosen ports; `--url`/`--ssh-port` override public mappings. Review
pairs public and node ports. For busy ports, interactive installs ask for another; unattended
commands refuse and name the flag. The profile defaults to `<context>-<namespace>`; `--profile`
selects another. Reruns, upgrades and rollback refuse without the original administrator key. Saved
namespaces and ports are reused. Without a release revision, explicit answers replace the saved
record; review shows changes. Pending revisions must match saved answers. Keep the profile while its
administrator Secret remains. Hand-made namespaces stay unmarked and outside removal; `default` and
`kube-*` are forbidden. Compatible foreign-owned definitions stay unchanged; review names owners.
Required schema fields and served/storage versions must match our pinned controller. Controller and
workflows use the server namespace as Argo instance ID; default Argo controllers skip them. Our
controller watches only its pipeline namespace. Cluster-wide permission and policy names include
custom namespaces. Rollback preserves shared workflow definitions.
Removing the Argo release keeps them by resource policy; every run object depends on those
definitions. Upgrading with chart defaults replaces the definitions' contents; use versions the
release notes name as compatible. Install and upgrade refuse workflow definitions that would drop
fields this version submits; after their owner upgrades them, run `oberth upgrade --dry-run`.
## Keep the recovery files
Copy `~/.config/oberth/<profile>/`, archives, `backup.agekey` and `recovery` off-machine privately.
`installation.json` records paths. Complete backups encrypt the five installation Secrets and
bundled store data; keep the age identity, administrator private file and recovery material apart.
Missing administrator keys refuse install. `OBERTH_PROFILE` or `~/.config/oberth/current` selects;
agent joining preserves selection.
## Agent plugins
Once public, add the Claude Code skill and run box with `/plugin marketplace add oberthci/oberth`
and `/plugin install oberth@oberthci`. Default: fetcher's `~/.local/bin/oberth`, `OBERTH_PROFILE` or
joined checkout profile. Elsewhere, use join's `agent tools: stdio MCP server "oberth"` line:
```sh
claude plugin configure oberth@oberthci --values-stdin <<'JSON'
{"executable":"/absolute/verified/path/from-join","profile":"uplink-profile-from-join","runs":3}
JSON
```
Options take precedence; blank means unset. No profile hides the box. Use the verified executable,
never a checkout file. While a run is active or ended under 10 minutes ago, the box shows up to
`runs` runs (0 to 10; blank is three; none at 0): id, repository, ref, status, steps, time, age,
failed step. `/oberth-runs [--limit=N]` adds why a run failed or was cancelled;
`/oberth-watch <id-or-prefix>` toasts on completion; the model uses the bridge's tools. Update:
`claude plugin marketplace update oberthci`, `claude plugin update oberth@oberthci`, restart. Codex:
`codex plugin marketplace add oberthci/oberth`, then `codex plugin add oberth@oberthci` (skill:
`agent/.codex-plugin/plugin.json`). Register join's bridge line exactly with
`codex mcp add oberth -- <line>` ([setup](agents.md#connect-the-tools)). Enable Custom status line
in `/experimental`, restart, then choose `Oberth runs` in `/statusline`: the box's cells for three
recent runs, refreshed every 15 seconds. No plugin commands or completion toasts; ask for runs.
## Upgrade
When the review shows a schema change, take a storage-level copy of the data volume before applying.
```sh
oberth upgrade --dry-run
oberth upgrade --yes
```
Upgrades require signed releases; local charts use `oberth install --chart <directory>`. After
upgrading, replace the CLI with that release's verified client and repoint wrappers. Add
`--version <version>` to both commands to select it; `--service-port 443` selects HTTPS port 443
(default 8443). Offline `--release-dir <directory>` reads signed files or a parent of version
directories; version directories override flat files and invalid directories refuse. Same-schema
repair needs both releases; select their parent. Review preserves settings and shows changed
defaults, schema, revision and images (JSON). Install/upgrade replace direct Helm edits;
rollback/repair refuse them and name install or upgrade.
Review failed revisions. Finish unlabelled Workflows before upgrading (not adopted). Runs survive
restarts; submission during a controller restart may fail at step start: `oberth run rerun <id>`.
Database backups stay in the profile by version/UTC time; copy off-machine. If the store restarts
sealed, run `oberth unseal`, then repeat the upgrade. Success keeps the three newest copies; failure
keeps recovery files. The record binds cluster UID, revisions, versions and backup digest;
`--no-backup` opts out. Completion/rollback audit versions and revision. Missing receipts do not
undo success; use the printed audit command. Readiness waits up to ten minutes. After a timeout,
wait for `oberth system status` to be ready, then run `oberth upgrade`, which completes a ready
target's record unchanged, or `oberth rollback --yes`. After a schema change, if it stays down,
recovery is manual: keep the encrypted pre-upgrade archive, `backup.agekey` and `rollback.json` in
`~/.config/oberth/<profile>/` and copy them off-machine.
Before downgrading, cancel non-terminal resumed runs or wait for them to finish.
An orphaned resumed Workflow needs its `oberth.ci/record` finalizer removed by hand.
## Roll back
Review the saved record with `oberth rollback --dry-run`, then run `oberth rollback --yes` with its
original profile. A different schema, cluster UID or unexpected revision refuses before writes.
Repeating resumes rollback; without the record, inspect the deployed revision. Offline, use
`--release-dir <directory>` with signed source-version files; another version refuses. Before
rolling v0.30.19 back to v0.30.18, pause affected schedules, remove their YAML declarations and
verify their CronWorkflows are gone. Finish or cancel every child and verify collection before
downgrading. Recreate schedules with the restored release before resuming; startup sync can fail.
## Back up
`oberth backup --out <new-archive-file>` encrypts the database, bundled store data and five
installation Secrets: CA key, forge deploy key, SSH host key, store TLS and administrator public
login keys. The writer checks members while encrypting with the public age recipient.
`oberth-backup-signing` holds that recipient (ConfigMap mismatches refuse) and signs
`<archive>.manifest.json` (name, hashes, digest); the profile keeps `backup_public_key`. Copy both
off-machine unrenamed; save SHA256 separately. Use local hard links; never overwrite. Limit: 1 GiB;
`--dry-run` previews. `--database-only` and pre-upgrade copies exclude store data and installation
Secrets. `oberth restore --from <archive> --identity-file <private-age-file> --dry-run` reviews
offline; `oberth backup verify` uses the same inputs. Both need `--backup-profile <name>` or saved
`--sha256 <saved-digest>`. Restore with the same or a newer release:
v0.30.17 cannot read archives with forge host pins. Keep age, administrator and unseal files apart:
the archive has neither age identity nor unseal material. A database-only backup cannot restore
store values; keep secure originals to put again. Separate `recovery` can unseal data.
`oberth access list --repo <repo> --kind store_path --json` lists paths/steps, never values. JSON
warnings exit 0: optional `warning` has `code`, `reason`, `remedy`, optional `id`, `context`, `detail`;
also on standard error; clean and dry-run results omit `warning`.
`oberth backup --directory <dir> --keep 7` prunes verified signed full copies after success
(1–1000), never unsigned copies, other names or links; `--database-only` refuses `--keep`. Signed
`installation_id` is the backup signing public key. Foreign archives refuse without
`--allow-shared-directory`; sharing prunes only this installation's copies.
`oberth backup --print-schedule` prints [CronJob and scoped RBAC](backup-schedule.yaml). Edit image,
PVC and schedule; apply in the Restricted server namespace. No profile, kubeconfig or decryption key
is mounted. The Job retries 503s six times (1–16s waits), with two Pod retries.
`oberth system status` shows backup job (including missed run) and backup last success. Bundled
backups need store access. Rollback keeps `oberth-backup`; schema changes fail the Job until
upgrade. For store access via a saved share, set listener
`disable_unauthed_generate_root_endpoints=false`, restart/unseal, run:
`oberth secretstore setup --backup-only --config <config> --ca-file <ca> --unseal-file <share>` OTP
stays in memory. Public `<config>.backup-setup.json` records store/nonce before root generation. Use
a writable persistent config directory; copy a read-only mounted config there. CA/share mounts can
stay. Rejected or unfinished attempts need cancellation only. Uncertain replies block setup.
Revocation removes the receipt. `DELETE sys/generate-root/attempt` cancels unfinished generation
only. For uncertain issuance, obtain a separate native quorum session; keep OTP/reply private.
`LIST auth/token/accessors`; `POST auth/token/lookup-accessor` with each `accessor`. Coordinate with
root-session owners; `POST auth/token/revoke-accessor` for all other root-policy accessors. Revoke
cleanup access last (`POST auth/token/revoke-self`), remove the receipt, retry, restore listener
`true`, restart/unseal. Lost issued replies/accessors cannot be recovered by nonce.
[Native API](https://openbao.org/docs/api/system/generate-root/). Or use `--token-file <session>`
and revoke it afterward. Grant read/create/update only on `sys/policies/acl/oberth-backup` and
`auth/<auth-mount>/role/oberth-backup`; drift refuses. Restore needs a signed
matching-schema/protocol release (`--release-dir` works offline), fresh cluster, new profile and the
same namespaces:
```sh
oberth restore --from <archive> --identity-file <age> --backup-profile <original> \
  --context <fresh-context> --profile <new-profile> --url <url> \
  --admin-key-file <admin> --unseal-file <share> --yes
```
The server stays stopped until restore passes; five Secrets and values stay. Occupied targets refuse
even with `--yes`. After failure, keep the volume, remove the partial target and use a new profile.
Bundled archives need a snapshot; external-store recovery is operator-owned.
## Unseal after restart
After restart, run `oberth unseal` with the selected profile's `recovery`; never print it. Install
configures the bundled store's static rules/audience. External stores need an operator Pod: run
`oberth secretstore check` or `oberth secretstore setup` with `--config` (server/audience),
`--ca-file` (CA), `--token-file` (private OpenBao operator token). Check reads auth/policies/roles;
setup needs configure access. The administrator profile lacks these files. A Kubernetes login token
is not an OpenBao operator token; external operators supply one and run setup before use.
## Remove an installation
There's no uninstall command: use Helm directly. Set `context`/`namespace`, stop runs and save data,
secret names, values, recovery files and archives privately. Install/upgrade/rollback with Oberth.
First `kubectl delete -f` an applied backup schedule; the chart owns its ServiceAccount.
```sh
helm --kube-context "$context" uninstall oberth -n "$namespace" --wait
```
Helm's pre-delete hook releases this installation's archive finalizers; keep hooks enabled. Rollback
releases them too. Copy needed logs first. Helm removes server, controller, store, permissions,
policies and its installation-created pipeline namespace. Existing namespaces stay. These objects
remain in the server namespace; Helm never owned the five Secrets:

| Object | What remains |
| --- | --- |
| PVC `oberth-data` | Server database; Helm's resource policy keeps it. |
| PVC `data-oberth-store-0` | Bundled store data; its StatefulSet leaves this claim behind. |
| Secret `oberth-admin` | Administrator public keys used to authorize access. |
| Secret `oberth-ca` | Installation CA certificate and private signing key. |
| Secret `oberth-forge-key` | Forge deploy private key and public key. |
| Secret `oberth-ssh-host` | Server SSH host private key and public key. |
| Secret `oberth-store-tls` | Store TLS certificate and private key. |
| Backup ConfigMap and Secret | `oberth-backup` settings; `oberth-backup-signing` private key. |

Save data and keys; choose ONE cleanup. Keep volumes for data; storage class controls reclamation.
Shared workflow definitions stay, including ours; deleting them deletes every installation's runs.
### Installation-created namespace
Pre-existing namespaces stay. Namespace deletion removes everything; first check other workloads.
Created namespaces need matching Helm creation marks and no remaining release Secrets:
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
Keep a pre-existing shared namespace; remove only this installation's claims, Secrets and ConfigMap:
```sh
kubectl --context "$context" -n "$namespace" delete pvc data-oberth-store-0 oberth-data
kubectl --context "$context" -n "$namespace" delete secret oberth-backup-signing \
  oberth-admin oberth-ca oberth-forge-key oberth-ssh-host oberth-store-tls
kubectl --context "$context" -n "$namespace" delete configmap oberth-backup
```
### Retained pipeline namespace
A pre-existing pipeline namespace keeps step ServiceAccounts and run objects after Helm removal. Set
`pipeline_namespace` to the reviewed name; remove only by these labels, never by name patterns:
```sh
instance="workflows.argoproj.io/controller-instanceid=$namespace"
kubectl --context "$context" -n "$pipeline_namespace" delete cronworkflows.argoproj.io \
  -l "app.kubernetes.io/managed-by=oberth,$instance"
kubectl --context "$context" -n "$pipeline_namespace" delete workflows.argoproj.io \
  -l "oberth.ci/engine=argo,$instance"
kubectl --context "$context" -n "$pipeline_namespace" delete serviceaccounts \
  -l app.kubernetes.io/managed-by=oberth
```
Selectors keep other runs; Workflow deletion removes dependent Pods. Keep the pipeline namespace.
### Local profile
After preserving its recovery material and archives, remove the local profile using its path:
```sh
rm -r -- /absolute/path/to/installation-profile
```
This removes credentials, recovery material, records and backups. Select a live `OBERTH_PROFILE`.
## Command counts
Counts include the [first green build](../README.md#first-green-build), refused registration and
verified retry; a required environment variable adds one flag.

| First green path | Commands | Flags |
| --- | --- | --- |
| Terminal, default ports: install with `--context` | 8 | 3 |
| Unattended, mapped ports: apply only, with `--yes --url --ssh-port` | 8 | 6 |
| Agent, mapped ports: address and SSH port on both commands | 9 | 10 |

`repo add` reads the Oberth server's SSH keys from the profile; the forge pin is server-owned. A dry
run adds one command and its flags, included in the agent row. The three release commands assume no
refusals; missing grants add approval commands and another release attempt. `secret put` takes three
flags.
