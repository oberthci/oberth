# Backup and restore

`oberth backup` writes one encrypted file with an installation's history,
secrets and settings; `oberth restore` puts that state onto an installed
(typically fresh) Oberth. Both run on an operator workstation, like
`oberth install`: they need `kubectl`, `helm` and OpenSSL 3, and reach the
cluster only through the Kubernetes API and `kubectl exec`.

## Quick start

On a machine with kubectl access to the cluster:

```sh
oberth backup        # asks for the OpenBao root token; Enter alone = history only
```

This writes `./oberth-backup-<YYYYMMDD-HHMMSS>.tar.gz.enc` (UTC time) in the
current directory. The first backup on a machine also creates the key it is
encrypted to, `~/.config/oberth/backup/recipient.key`: keep a copy of that
key **off the machine**, because no backup can be restored without it.

After reinstalling, from the directory that holds the backup:

```sh
oberth restore       # the newest backup here; shows what changes, then asks you to confirm
```

`oberth restore --inspect` shows what a backup holds without touching a
cluster, and `oberth restore --dry-run` shows what would change on this
install without changing it.

## Backup

```sh
oberth backup                   # history, secrets and settings
oberth backup --history-only    # history only: no secrets, no token
oberth backup --dry-run         # what would be backed up; reads no secrets, writes nothing
```

A full backup asks for the OpenBao root token. It takes `BAO_TOKEN` from the
environment if set (`read -rs BAO_TOKEN && export BAO_TOKEN`), otherwise it
prompts with echo off. Pressing Enter alone at the prompt makes a
history-only backup instead. Without a terminal and without `BAO_TOKEN` a
full backup stops and points at `--history-only`.

Flags:

- `--output FILE` / `-o FILE`: write here instead of
  `./oberth-backup-<date-time>.tar.gz.enc`. The file must not exist; it is
  created with mode 0600.
- `--history-only`: runs, issues, promotions, uplinks and step logs only.
- `--no-logs`: leave the step logs out.
- `--dry-run`: print what would be collected; reads nothing from OpenBao and
  writes no file.

Advanced: `--recipient CERT` encrypts to another PEM certificate instead of
`~/.config/oberth/backup/recipient.pem` (see [Encryption](#encryption));
`--context`, `--kubeconfig`, `--namespace` (default `oberth`),
`--openbao-namespace` (default `openbao`), `--argo-namespace` (default
`oberth-argo`).

When it finishes, `oberth backup` prints the file, its size, the history it
holds, the secret counts and settings, the verified database schema and audit
head, which key restores it, and the file's SHA-256.

Backup changes nothing in the cluster. The database snapshot is taken by the
running server binary itself (`oberth backup database`, run through
`kubectl exec`): it copies the database and WAL privately, refuses if they
changed meanwhile, and verifies integrity, foreign keys, the schema identity
and the audit chain under the deployment's own `OBERTH_AUDIT_DOMAIN` before
streaming the result out. A server released before that subcommand existed
gets a stable copy instead: both files are streamed out and must hash
identically before and after, and the same verified snapshot is then built on
the workstation. A snapshot that races a write is retried up to three times.
Step logs are read as a tar of `/data/logs` through the server pod; logs above
160 MiB are left out with a note. Only regular files directly in `logs/` are kept
(the server's in-flight `.index-*.tmp` files are dropped), and logs that cannot
be read leave the backup without logs rather than failing it.
If `recipient.pem` exists without its `recipient.key`, the backup refuses:
everything encrypted to it would be unrecoverable.

To learn which key signs the cluster's service-account tokens, backup
requests a short-lived token for the namespace's `default` service account and
reads only its header; TokenRequest stores nothing.

The compatibility values recorded are the deployment's live
`OBERTH_SCHEMA_IDENTITY`, `OBERTH_AUDIT_DOMAIN` and `OBERTH_WITNESS_KEY_INFO`.
The database is verified under them before the file is written, so a restore
never has to guess the audit domain.

### What a backup holds

| Member | Full | History only | Contents |
| --- | --- | --- | --- |
| `manifest.json` | yes | yes | format, scope, times, versions, source cluster, counts, member hashes (below) |
| `openbao/kv.json` | yes | – | every KV v2 secret under the `oberth` mount, current version |
| `openbao/policies.json` | yes | – | every ACL policy except `root` and `default` |
| `openbao/roles.json` | yes | – | every `auth/kubernetes` role with **every** field OpenBao returns |
| `openbao/reference.json` | yes | – | Kubernetes auth config, mount list, Transit key metadata, PKI CA certificate; reference only, never restored |
| `database/oberth-database.tar` | yes | yes | verified server database snapshot (`oberth.database-backup/v1`): runs, issues, promotions, repositories, uplinks and their tokens, the audit chain |
| `kubernetes/helm-values.json` | yes | – | the `oberth` release's user-supplied Helm values |
| `kubernetes/secret-access.json` | yes | – | the `oberth-secret-access` ConfigMap (the grants) |
| `server/logs.tar` | optional | optional | the server's step logs (`/data/logs`); absent with `--no-logs`, when there are none, or above 160 MiB |

`manifest.json` comes first and records the format (`oberth.backup/v2`), the
scope (`full` or `history`), the creation time, the CLI, server and chart
versions, the source context, API server, namespaces and server image, the
source cluster UID (the `kube-system` namespace UID) and service-account key
IDs, the database schema, audit head and snapshot method, the compatibility
values, the history and OpenBao counts, and the size and SHA-256 of every
member. It holds no secret material. A restore refuses any file whose members
do not match it exactly. `oberth.backup/v1` files written by v0.17.31 (an
uncompressed tar, no step logs, no cluster identity) are still read.

### What is not backed up

- **Git caches, workspaces and artifacts** on the server volume. The server
  clones each repository from its forge again on first use; a restore onto an
  existing volume leaves them in place.
- **Transit keys** are created non-exportable. Only their metadata is recorded.
  The installer creates a new key on the target, so trusted-plan envelopes
  sealed under the old key cannot be decrypted after a restore.
- **The server PKI CA** key is generated inside OpenBao and never leaves it.
  Only the CA certificate is recorded. The new install has its own CA; every
  CLI and MCP client must re-pin it.
- **The Kubernetes auth configuration** names the source cluster's API CA and
  issuer. It is recorded for reference and never restored; the installer
  writes the target's.
- **KV history**: secrets are captured at their current version only.
- **The cluster's service-account signing key** (on k3s,
  `/var/lib/rancher/k3s/server/tls/service.key`) lives on the Kubernetes
  control plane, out of reach of the API. Release Workload Identity Federation
  trusts it; see [Keep the service-account key](#keep-the-service-account-key).

### Your own Helm values

A restore brings back what Oberth needs to run; deployment-specific
settings from your values files (an extra integration, a network policy, a
replica count) are yours to re-apply with `oberth upgrade -f FILE`. Secrets
those settings rely on are ordinary OpenBao secrets and come back with the
`secrets` part. When the values files themselves are lost:

```sh
oberth backup export-values > values.yaml     # the newest backup here
```

prints the Helm values the backup recorded. Values the installer sets itself
describe the source install: keep only your own settings before
`oberth upgrade -f values.yaml`.

### Export the secrets as plain JSON

```sh
oberth backup export-secrets > secrets.json                # the newest backup here
oberth backup export-secrets FILE --output secrets.json   # created 0600
```

This writes the KV secrets a full backup holds as one JSON document (format
`oberth.secrets/v1`: the mount, then each KV v2 path with its key/value data),
which any OpenBao or Vault can import with its own CLI. It needs no cluster.
`FILE` defaults to the newest backup, found as `oberth restore` finds it; the
key is found the same way, or pass `--key KEY`. The output is **not
encrypted**: the command refuses to print to a terminal, and `--output`
creates a new file with mode 0600. Delete it once imported. A history-only
backup has no secrets to export.

Import into a KV v2 mount with the `bao` (or `vault`) CLI:

```sh
jq -c '.secrets[]' secrets.json | while read -r s; do
  echo "$s" | jq .data | bao kv put -mount=oberth "$(echo "$s" | jq -r .path)" -
done
```

## Encryption

The file is a gzip-compressed tar stream sealed with `openssl cms`
AuthEnvelopedData: AES-256-GCM content encryption, RSA-OAEP-SHA256 key
transport to one recipient certificate. This is the construction
`cmd/oberth-store-backup` uses (see [database-backup.md](database-backup.md));
no custom format is involved. The recipient key must be RSA with at least
3072 bits. A restore refuses anything that is not AuthEnvelopedData (in
particular unauthenticated CBC EnvelopedData) before decrypting, and parses
plaintext only after OpenSSL has authenticated the whole file and exited
successfully.

Anyone holding the public certificate can produce a file the private key
opens, so the encryption gives confidentiality, not authorship: keep backups
where only you can write them, and compare the SHA-256 that `oberth backup`
prints with the one `oberth restore` shows.

### The backup key

The first `oberth backup` on a machine creates an RSA-4096 `recipient.key`
(mode 0600) and its self-signed certificate `recipient.pem` in
`~/.config/oberth/backup/` (directory mode 0700). Every later backup is
encrypted to the same certificate. Existing files are never replaced: backups
already encrypted to a key need it.

Keep an independently protected copy of `recipient.key` **off the machine**
(a password manager or another host). Without it no backup can be restored.

`oberth backup keygen` creates the pair ahead of time and is optional;
`--dir DIR` writes it elsewhere, and backups then need
`--recipient DIR/recipient.pem`. An equivalent OpenSSL command, for a
certificate to pass with `--recipient`:

```sh
umask 077
openssl req -x509 -newkey rsa:4096 -nodes -days 3650 \
  -subj /CN=oberth-backup -keyout recipient.key -out recipient.pem
```

`oberth restore` and `export-secrets` use `--key KEY` if given, else
`~/.config/oberth/backup/recipient.key`, else a `recipient.key` in the same
directory as the backup (for a backup and key carried to a new machine
together). They refuse a key readable by group or others.

### Handling of secrets

Plaintext never touches disk outside a private `0700` working directory that
is removed when the command returns; `export-secrets` is the one deliberate
exception. The root token is read from `BAO_TOKEN` (or an echo-off prompt),
removed from the process environment together with `VAULT_TOKEN`, and sent to
OpenBao only as the first stdin line of each `kubectl exec`; secret values
travel on stdin too. Neither ever appears in an argument list, which the API
server records in its exec audit log.

## Restore

Install Oberth on the target first (`oberth setup`, or `oberth install
--install-secretstore`), in the same namespaces as the source, with a server
release at least as new as the one that wrote the backup. Then, from the
directory with the backup:

```sh
oberth restore --inspect         # what the backup holds; no cluster needed
oberth restore --dry-run         # what would change on this install; changes nothing
oberth restore                   # restore it
oberth restore --only history    # runs, issues, step logs only; keeps this install's secrets and settings
```

Without an argument, restore takes the newest backup (`*.tar.gz.enc`, or a
`*.cms` file from an older release) in the current directory, then in
`~/oberth-backups` (where v0.17.31 wrote them), looking one subdirectory level
down as well. An argument may be a file, or a directory meaning the newest
backup in it. It says which file it picked.

Restoring secrets needs the **new** install's OpenBao root token: `BAO_TOKEN`
or an echo-off prompt. `--only history` needs none. `--dry-run` does not
prompt; export `BAO_TOKEN` first to have the plan compare secrets with this
install's, otherwise it lists them without comparing.

Flags:

- `--inspect`: decrypt and verify the backup, print what it holds (every
  repository, uplink and secret name; never secret values) and stop. Needs no
  cluster.
- `--dry-run`: print the contents and what would change on this install, then
  stop.
- `--only PARTS`: restore only these parts, comma-separated (below).
- `--yes`: skip the confirmation (for scripts); it needs the backup named explicitly (`oberth restore FILE --yes`).

Advanced: `--key KEY` (alias `--private-key`), `--input FILE` (alias `-i`,
same as the argument), `--no-logs` (do not restore the step logs),
`--restore-forge-key` (see [below](#the-forge-key-and-the-ssh-host-key)),
`--chart CHART` (a local chart for the Helm upgrade; default the published
chart at the version the release runs), `--timeout` (wait for pods), and the
cluster flags `--context`, `--kubeconfig`, `--namespace`,
`--openbao-namespace`, `--argo-namespace`.

### The parts

By default a restore applies every part the backup holds: all three for a
full backup, `history` for a history-only one. `--only` picks some of them;
naming `secrets` or `settings` for a history-only backup is refused.

| Part | What it restores |
| --- | --- |
| `history` | the database (runs, issues, promotions, repositories, uplinks and their tokens, the audit chain), replacing this install's rather than merging; the step logs; the compatibility values the restored database must be opened under; and deletion of Argo Workflows whose runs the restored database does not know |
| `secrets` | OpenBao KV secrets, ACL policies and `auth/kubernetes` roles, each verified by read-back; this install's forge key is kept unless `--restore-forge-key` |
| `settings` | the grants (`oberth-secret-access` ConfigMap), `argo.releaseWIF`, and the certificate names `tls.extraDNSNames`/`tls.extraIPs` |

Parts not restored are left as they are and listed as `Unchanged` in the
plan. Whatever the parts, the server is stopped while restoring and started
again.

### What you see before anything changes

Restore first decrypts the backup, checks every member against the manifest,
and validates the database offline with the store package: SQLite integrity,
foreign keys, the exact schema version, the schema identity, and the full
audit hash chain under the recorded audit domain, with the audit head checked
against the manifest. It then prints what the backup holds: when and where it
was taken, its SHA-256, the history counts, repositories, uplinks and step
logs, the secret names, and the settings.

Next it reads, without writing, what the chosen parts need from the target: the `oberth` Deployment and its
volume claim, the Helm release and its values, the running server's schema
version (it must be at least the backup's), the server's own history (from a
verified snapshot its running server takes), the current grants, OpenBao
(which must be unsealed) with its current secrets, policies and roles, the
forge key and SSH host key, the server CA, the cluster UID and
service-account keys, and the Argo Workflows.

Then it prints **What changes on this install**:

- **History**: replaced, not merged, as before -> after counts of runs,
  issues, promotions and repositories; uplinks removed (their tokens,
  including the one this install printed, stop working) and uplinks restored
  (their previous tokens work again); how many step log files go into
  `/data/logs`.
- **Secrets**: how many are new, replaced and unchanged, with the replaced
  and new paths by name; new and replaced policies and roles; what happens to
  the forge key and SSH host key.
- **Settings**: each one from its current value to the restored one:
  compatibility values, `argo.releaseWIF`, `tls.extraDNSNames`,
  `tls.extraIPs`, and the grant count.

followed by **How**: the server stopped and started, the database file
replaced with this install's kept in `/data/pre-restore-<time>/`, the
workflows to delete, the Helm upgrade, and notes (a service-account key that
differs, settings deliberately not reapplied).

`--dry-run` stops here. Otherwise restore asks you to type the target
context's name (for k3s, `default`); a mistyped answer is asked again, up to
three times. Without a terminal it needs `--yes`.

### What the restore does

1. With `secrets`: writes every KV secret that differs from the target's
   (JSON on stdin), every policy and role, and reads each one back: secrets
   and policies must match exactly, roles field-for-field (deprecated aliases
   such as `policies` that mirror their `token_*` successor are not written
   twice, which OpenBao rejects). Any difference stops the restore before the
   server is touched.
2. When settings must change on the release, resolves the chart
   (`helm show chart`) before stopping anything, so a missing chart cannot
   leave the server scaled down.
3. Scales `deploy/oberth` to 0 through the scale subresource (so Helm's
   server-side apply keeps owning `.spec.replicas`) and waits for its pods to
   exit.
4. With `history`: starts a short-lived pod with the server's image, identity
   and placement that mounts the server's PersistentVolumeClaim — no host
   paths, so any storage class works — uploads the database over stdin,
   checks the size the pod received and re-checks the SHA-256 on the volume,
   moves `oberth.sqlite`, `-wal` and `-shm` into
   `/data/pre-restore-<UTC timestamp>/`, moves the restored files into place,
   and checks they are owned by the server user with mode 0600. A swap that
   fails midway puts the previous files back.
5. With `history`, unless `--no-logs`: uploads the step logs the same way and
   extracts them into `/data/logs` (only regular files directly in `logs/`
   are admitted; files of the same name are replaced). A failure here is
   reported and the restore continues: logs are history, not state.
6. With `settings`: restores the `oberth-secret-access` ConfigMap. The server
   reconciles grants from it at startup, so it must be in place before the
   server starts.
7. With `history`: lists the Argo Workflows again, now that no server can
   submit new ones, and deletes those whose `oberth.ci/run` label names a run
   the restored database does not know — they belong to the replaced database
   and would otherwise keep running and hold pipeline slots.
8. Sets on the release, with `helm upgrade --reuse-values` (published chart at
   the release's current version, or `--chart`), whatever differs of:
   `compatibility.{schemaIdentity,auditDomain,witnessKeyInfo}` (with
   `history`); `argo.releaseWIF`, unless the backup's was recorded for another
   pipeline namespace; and `tls.extraDNSNames`/`tls.extraIPs` as the union of
   the target's and the backup's names, so clients that used them keep
   working. Nothing else from the backup's Helm values is applied: the values
   the installer sets (image, OpenBao address and CA, PKI, service type,
   certificate pins) belong to the new install, and the rest are your own
   deployment settings. The plan names those that differ from this install's,
   to re-apply from your values files (see below).
9. Scales the server back up and waits for it to become ready. With the PKI
   enabled, the server issues itself a new leaf with every name at startup.

If a step after the scale-down fails, the error says whether the previous or
the restored database is in place and how to get back to a running server;
fix the cause and rerun the restore (every step is idempotent), or scale the
Deployment back up. The previous database stays in its `pre-restore-*`
directory until you remove it.

### Same cluster or another one

Every k3s serves its API at `https://127.0.0.1:6443`, so the API URL cannot
tell a rollback from a restore onto a reinstalled cluster. The plan compares
the `kube-system` namespace UID recorded in the backup with the target's: the
same UID means a rollback (runs created since the backup are dropped);
another UID means a reinstalled or new cluster. For v1 backups, which record
no UID, the plan says only whether the API URL matches.

### The forge key and the SSH host key

The forge (upstream) key is the SSH key Oberth uses to fetch from GitHub,
Codeberg and other forges (`identities/<namespace>/oberth-upstream-key`). The
install on the target registered its key with the forges and verified it, while
the backed-up key may have been removed from them since. So when the target
already holds a **different** forge key, the restore keeps the target's and
says so in the plan; `--restore-forge-key` restores the backed-up one instead.

The server's SSH host key (`identities/<namespace>/server`) **is** restored,
so workstations that knew the old server keep trusting it. A workstation that
connected to the fresh install before the restore sees "REMOTE HOST
IDENTIFICATION HAS CHANGED"; the plan names both fingerprints, and
`ssh-keygen -R '[<host>]:30022'` removes the stale entry.

### Keep the service-account key

Release Workload Identity Federation (`argo.releaseWIF`) has the cloud
provider trust the cluster's service-account JWKS. A reinstalled cluster signs
with a new key, so releases cannot authenticate until either the provider
trusts the new JWKS (`kubectl get --raw /openid/v1/jwks`) or the cluster signs
with the old key again. The plan compares the backup's key IDs with the
target's and says when they differ.

On k3s, keep the old key across a reinstall by saving
`/var/lib/rancher/k3s/server/tls/service.key` (and `service.current.key`)
before `k3s-uninstall.sh` and putting them back in place **before** k3s first
starts. If the new cluster is already running, do not swap the files: k3s
then refuses to start ("service.key newer than datastore"). Write the key into
the datastore instead, keeping the new key for verification so running pods
stay valid:

```sh
T=/var/lib/rancher/k3s/server/tls
P=$(sudo mktemp -d)                # sudo-owned, mode 0700
sudo mkdir "$P/tls"
sudo cp -p $T/client-ca.* $T/server-ca.* $T/request-header-ca.* "$P/tls/"
sudo cp -rp $T/etcd "$P/tls/"
# the OLD key first (it signs), then the current one (it still verifies)
sudo sh -c "cat /path/to/old/service.key $T/service.key > $P/tls/service.key"
sudo k3s certificate rotate-ca --force --path="$P"   # the CA files are unchanged copies
sudo systemctl restart k3s
kubectl get --raw /openid/v1/jwks                    # lists both key IDs
sudo rm -rf "$P"
```

`--force` is needed because the copied CA bundles are single self-signed
certificates; they are the current ones, so nothing but the service-account
key changes.

### After a restore

The restore prints these steps when it finishes.

1. Reconcile per-repo identities and policies from the restored grants, with
   your usual install flags:

   ```sh
   read -rs BAO_TOKEN && export BAO_TOKEN
   oberth install --install-secretstore --upgrade
   ```

   This also refreshes the pinned server CA on this machine.
2. Re-pin the new server CA on every other CLI and MCP client (rerun
   `oberth install` there, or copy `~/.config/oberth/ca.crt`).
3. Uplinks and their tokens come back with the database: the token the fresh
   install printed stops working, and the previous ones work again.
   Re-register any workstation whose key changed: `kubectl exec -i -n oberth deploy/oberth -- oberth uplink add - you@host < ~/.ssh/id_ed25519.pub`.
4. Re-run any trusted plan whose envelope was sealed under the old Transit key.
5. If the node runs the opt-in `deploy/systemd/oberth-openbao-unseal` watcher,
   give it the new install's unseal key: `sudo install -m 0400 -o root -g root
   <unseal-key-file> /etc/oberth/openbao-unseal-key && sudo systemctl restart
   oberth-openbao-unseal`.
6. If the plan noted a different service-account key, see
   [Keep the service-account key](#keep-the-service-account-key).
7. External audit witnesses (Rekor) may be ahead of the restored audit head;
   the server then refuses to start rather than rewrite history. Never delete
   witness records to make a restore pass; see
   [database-backup.md](database-backup.md).
