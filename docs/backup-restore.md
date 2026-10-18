# Backup and restore

A backup holds only what a fresh install cannot recreate:

- **the history**: the server database, with runs, issues, promotions,
  repositories, uplinks and their tokens, and the audit chain;
- **the secrets pipelines use**: the OpenBao KV secrets you put in, the ACL
  policies, the `auth/kubernetes` roles, and the grants (the
  `oberth-secret-access` ConfigMap).

Everything else belongs to the install and is made again by `oberth setup` /
`oberth install`: see [What is not backed up](#what-is-not-backed-up).

`oberth backup` writes this into one encrypted file; `oberth restore` puts it
onto an installed (typically fresh) Oberth. Both run on an operator
workstation, like `oberth install`: they need `kubectl`, `helm` and OpenSSL 3,
and reach the cluster only through the Kubernetes API and `kubectl exec`.

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
oberth backup                   # history and secrets
oberth backup --history-only    # history only: no secrets, no token
oberth backup --dry-run         # what would be backed up; reads no secrets, writes nothing
```

A full backup needs the OpenBao root token. It takes `BAO_TOKEN` from the
environment if set (`read -rs BAO_TOKEN && export BAO_TOKEN`), otherwise it
prompts with echo off. Pressing Enter alone at the prompt makes a
history-only backup instead. Without a terminal and without `BAO_TOKEN` a
full backup stops and points at `--history-only`.

Flags:

- `--output FILE` / `-o FILE`: write here instead of
  `./oberth-backup-<date-time>.tar.gz.enc`. The file must not exist; it is
  created with mode 0600.
- `--history-only`: the database only. OpenBao is not read.
- `--dry-run`: print what would be collected; reads nothing from OpenBao and
  writes no file.

Advanced: `--recipient CERT` encrypts to another PEM certificate instead of
`~/.config/oberth/backup/recipient.pem` (see [Encryption](#encryption));
`--context`, `--kubeconfig`, `--namespace` (default `oberth`),
`--openbao-namespace` (default `openbao`), `--argo-namespace` (default
`oberth-argo`).

When it finishes, `oberth backup` prints the file and its size, the history
it holds, the secret, policy, role and grant counts, the verified database
schema and audit head, which key restores it, and the file's SHA-256.

Backup changes nothing in the cluster. The database snapshot is taken by the
running server binary itself (`oberth backup database`, run through
`kubectl exec`): it copies the database and WAL privately, refuses if they
changed meanwhile, and verifies integrity, foreign keys, the schema identity
and the audit chain under the deployment's own `OBERTH_AUDIT_DOMAIN` before
streaming the result out. A server released before that subcommand existed
gets a stable copy instead: both files are streamed out and must hash
identically before and after, and the same verified snapshot is then built on
the workstation. A snapshot that races a write is retried up to three times.

The backup also records the deployment's live compatibility values
(`OBERTH_SCHEMA_IDENTITY`, `OBERTH_AUDIT_DOMAIN`, `OBERTH_WITNESS_KEY_INFO`).
The database is verified under them before the file is written, so a restore
never has to guess them.

If `recipient.pem` exists without its `recipient.key`, the backup refuses:
everything encrypted to it would be unrecoverable.

### What a backup holds

| Member | Full | History only | Contents |
| --- | --- | --- | --- |
| `manifest.json` | yes | yes | format, scope, versions, source cluster, counts, member hashes (below) |
| `openbao/kv.json` | yes | – | every KV v2 secret under the `oberth` mount except `identities/`, current version |
| `openbao/policies.json` | yes | – | every ACL policy except `root` and `default` |
| `openbao/roles.json` | yes | – | every `auth/kubernetes` role with every field OpenBao returns |
| `openbao/reference.json` | yes | – | Kubernetes auth config, mount list, Transit key metadata, PKI CA certificate; for reference, never restored |
| `database/oberth-database.tar` | yes | yes | verified database snapshot (`oberth.database-backup/v1`) |
| `kubernetes/secret-access.json` | yes | – | the `oberth-secret-access` ConfigMap (the grants) |

`manifest.json` comes first and records the format (`oberth.backup/v3`), the
scope (`full` or `history`), the creation time, the CLI, server and chart
versions, the source context, API server, namespaces and server image, the
source cluster UID (the `kube-system` namespace UID), the database schema,
audit head and snapshot method, the compatibility values, the history and
OpenBao counts, and the size and SHA-256 of every member. It holds no secret
material. A restore refuses any file whose members do not match it exactly.

Older `oberth.backup/v1` and `v2` files still restore. The Helm values and
step logs they carry are ignored, and the install identities they carry
(under `identities/`) are left out; `--inspect` says how many.

### What is not backed up

- **Server files**: git caches, workspaces, artifacts and step logs on the
  server volume. The server clones each repository from its forge again on
  first use.
- **Helm values.** Re-apply your own values files after a restore; see
  [After a restore](#after-a-restore).
- **The install's identities** under `identities/` in OpenBao: the forge
  (upstream) key, the server TLS and SSH host keys, and the forge
  `known_hosts`. Every install makes its own; the target keeps them.
- **The Transit key** is created non-exportable; only its metadata is
  recorded. The target has its own key, so trusted-plan envelopes sealed
  under the old one cannot be opened after a restore.
- **The server PKI CA** key never leaves OpenBao; only the CA certificate is
  recorded. The target has its own CA, which clients must re-pin.
- **The Kubernetes auth configuration** names the source cluster's API CA and
  issuer. It is recorded for reference only; the installer writes the
  target's.
- **KV history**: secrets are captured at their current version only.
- **The cluster's service-account signing key** (on k3s,
  `/var/lib/rancher/k3s/server/tls/service.key`) lives on the Kubernetes
  control plane; see [Keep the service-account key](#keep-the-service-account-key).

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
oberth restore --only history    # runs, issues, uplinks only; keeps this install's secrets
```

Without an argument, restore takes the newest backup (`*.tar.gz.enc`, or a
`*.cms` file from an older release) in the current directory, then in
`~/oberth-backups`, looking one subdirectory level down as well. An argument
may be a file, or a directory meaning the newest backup in it. It says which
file it picked.

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
- `--only PART`: restore only `history` or `secrets` (below).
- `--yes`: skip the confirmation (for scripts); it needs the backup named
  explicitly (`oberth restore FILE --yes`).

Advanced: `--key KEY` (alias `--private-key`), `--input FILE` (alias `-i`,
same as the argument), `--chart CHART` (a local chart for the Helm upgrade;
default the published chart at the version the release runs), `--timeout`
(wait for pods), and the cluster flags `--context`, `--kubeconfig`,
`--namespace`, `--openbao-namespace`, `--argo-namespace`.

### The parts

By default a restore applies every part the backup holds: both for a full
backup, `history` for a history-only one (where `--only secrets` is refused).

| Part | What it restores |
| --- | --- |
| `history` | the database, replacing this install's rather than merging; the compatibility values the restored database must be opened under; and deletion of Argo Workflows whose runs the restored database does not know |
| `secrets` | the OpenBao KV secrets (never `identities/`), ACL policies and `auth/kubernetes` roles, each verified by read-back; and the grants (`oberth-secret-access` ConfigMap) |

The compatibility values (`compatibility.schemaIdentity`, `auditDomain`,
`witnessKeyInfo`) are the only Helm values a restore sets. Whatever the
parts, the server is stopped while restoring and started again.

### What you see before anything changes

Restore first decrypts the backup, checks every member against the manifest,
and validates the database offline: SQLite integrity, foreign keys, the exact
schema version, the schema identity, and the full audit hash chain under the
recorded audit domain, with the audit head checked against the manifest. It
then prints what the backup holds: when and where it was taken, its SHA-256,
the history counts, repositories and uplinks, and the secret names.

Next it reads, without writing, what the chosen parts need from the target:
the `oberth` Deployment and its volume claim, the Helm release, the running
server's schema version (it must be at least the backup's), the server's own
history (from a verified snapshot its running server takes), the cluster UID,
the Argo Workflows, the current grants, and OpenBao (which must be unsealed)
with its current secrets, policies and roles.

Then it prints **What changes on this install**:

- **History**: replaced, not merged, as before -> after counts of runs,
  issues, promotions and repositories; uplinks removed (their tokens,
  including the one this install printed, stop working) and uplinks restored
  (their previous tokens work again).
- **Secrets**: how many are new, replaced and unchanged, with the new and
  replaced paths by name; new and replaced policies and roles; the grant
  count before -> after.
- **Unchanged**: the parts not restored, and always this install's
  identities, CA and Helm values.

followed by **How**: the server stopped and started, the database file
replaced with this install's kept in `/data/pre-restore-<time>/`, the
compatibility values set on the release if they differ, the workflows to
delete, and the read-back of OpenBao.

`--dry-run` stops here. Otherwise restore asks you to type the target
context's name (for k3s, `default`); a mistyped answer is asked again, up to
three times. Without a terminal it needs `--yes`.

### What the restore does

1. If the compatibility values differ, resolves the chart
   (`helm show chart`) before changing anything, so a missing chart cannot
   leave the server scaled down.
2. With `secrets`: writes every KV secret that differs from the target's
   (JSON on stdin), every policy and role, and reads each one back: secrets
   and policies must match exactly, roles field-for-field. Any difference
   stops the restore before the server is touched.
3. Scales `deploy/oberth` to 0 through the scale subresource (so Helm's
   server-side apply keeps owning `.spec.replicas`) and waits for its pods to
   exit.
4. With `history`: starts a short-lived pod with the server's image, identity
   and placement that mounts the server's PersistentVolumeClaim (no host
   paths, so any storage class works), uploads the database over stdin,
   checks the size the pod received and re-checks the SHA-256 on the volume,
   moves `oberth.sqlite`, `-wal` and `-shm` into
   `/data/pre-restore-<UTC timestamp>/`, moves the restored files into place,
   and checks they are owned by the server user with mode 0600. A swap that
   fails midway puts the previous files back.
5. With `secrets`: writes the `oberth-secret-access` ConfigMap. The server
   reconciles grants from it at startup, so it must be in place before the
   server starts.
6. With `history`: lists the Argo Workflows again, now that no server can
   submit new ones, and deletes those whose `oberth.ci/run` label names a run
   the restored database does not know: they belong to the replaced database
   and would otherwise keep running and hold pipeline slots.
7. With `history`, if they differ: sets the compatibility values with
   `helm upgrade --reuse-values` (published chart at the release's current
   version, or `--chart`). If Helm reports a field conflict on
   `.spec.replicas`, it retries once with `--force-conflicts`.
8. Scales the server back up and waits for it to become ready.

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
another UID means a reinstalled or new cluster. For backups that record no
UID, the plan says only whether the API URL matches.

### After a restore

The restore prints these steps when it finishes.

1. With `secrets`: recreate the per-repository identities pipelines read
   secrets with, from the restored grants, with your usual install flags
   (for example `--tls-extra-dns-name` / `--tls-extra-ip`):

   ```sh
   read -rs BAO_TOKEN && export BAO_TOKEN
   oberth install --install-secretstore --upgrade
   ```

2. Re-apply your own Helm values files, if you use any:
   `oberth upgrade -f FILE`.
3. With `history`: uplinks and their tokens are the backup's again. The token
   the fresh install printed no longer works; the previous ones do.
4. This install keeps its own server CA and SSH host key: other machines
   re-pin `~/.config/oberth/ca.crt` and drop a stale `known_hosts` entry
   (`ssh-keygen -R '[<host>]:30022'`).
5. If the node runs the opt-in `deploy/systemd/oberth-openbao-unseal`
   watcher, give it this install's unseal key:

   ```sh
   sudo install -m 0400 -o root -g root <unseal-key-file> /etc/oberth/openbao-unseal-key
   sudo systemctl restart oberth-openbao-unseal
   ```

External audit witnesses (Rekor) may be ahead of the restored audit head; the
server then refuses to start rather than rewrite history. Never delete witness
records to make a restore pass; see [database-backup.md](database-backup.md).

## Keep the service-account key

This is a general Kubernetes note, not something backup or restore handles.
Release Workload Identity Federation (`argo.releaseWIF`) has the cloud
provider trust the cluster's service-account JWKS. A reinstalled cluster signs
with a new key, so releases cannot authenticate until either the provider
trusts the new JWKS (`kubectl get --raw /openid/v1/jwks`) or the cluster signs
with the old key again. `argo.releaseWIF` itself is one of your Helm values.

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
