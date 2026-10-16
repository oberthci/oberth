# Backup and restore

`oberth backup` writes one encrypted archive of an installation's state;
`oberth restore` rebuilds that state on an installed (typically fresh) Oberth.
Both run on an operator workstation, like `oberth install`: they need
`kubectl`, `helm`, OpenSSL 3 and the OpenBao root token, and reach the cluster
only through the Kubernetes API and `kubectl exec`.

## What is in an archive

| Member | Contents | Restored |
| --- | --- | --- |
| `openbao/kv.json` | every KV v2 secret under the `oberth` mount, current version | yes, then read back and compared |
| `openbao/policies.json` | every ACL policy except `root` and `default` | yes, then read back and compared |
| `openbao/roles.json` | every `auth/kubernetes` role with **every** field OpenBao returns | yes, field-for-field, then diffed |
| `openbao/reference.json` | Kubernetes auth config, mount list, Transit key metadata, PKI CA certificate | no — reference only |
| `database/oberth-database.tar` | verified server database snapshot (`oberth.database-backup/v1`) | yes, swapped on the volume |
| `kubernetes/helm-values.json` | the `oberth` release's user-supplied Helm values | `compatibility` and `argo.releaseWIF` only |
| `kubernetes/secret-access.json` | the `oberth-secret-access` ConfigMap | yes |

`manifest.json` comes first and records the format (`oberth.backup/v1`), the
creation time, the CLI and server versions, the source context and API server,
the database schema and audit head, the compatibility values, and the size and
SHA-256 of every member. It holds no secret material. A restore refuses any
archive whose members do not match it exactly.

### What cannot be backed up

- **Transit keys** are created non-exportable. Only their metadata is recorded.
  The installer creates a new key on the target, so trusted-plan envelopes
  sealed under the old key cannot be decrypted after a restore.
- **The server PKI CA** key is generated inside OpenBao and never leaves it.
  Only the CA certificate is recorded. The installer issues a new CA on the
  target; every CLI and MCP client must re-pin it.
- **The Kubernetes auth configuration** names the source cluster's API CA and
  issuer. It is recorded for reference and never restored; the installer
  writes the target's.
- **KV history**: secrets are captured at their current version only.
- **Git caches, workspaces, logs and artifacts** on the server volume are not
  part of the archive. A restore leaves them in place; because the Git cache
  survives, re-pushing a branch it already holds is "up to date" — push a new
  commit to get a CI run on the restored state.

## Encryption

The archive is a tar stream sealed with `openssl cms` AuthEnvelopedData:
AES-256-GCM content encryption, RSA-OAEP-SHA256 key transport to one recipient
certificate. This is the construction `cmd/oberth-store-backup` uses
(see [database-backup.md](database-backup.md)); no custom format is involved.
The recipient key must be RSA with at least 3072 bits. A restore refuses
anything that is not AuthEnvelopedData (in particular unauthenticated CBC
EnvelopedData) before decrypting, and parses plaintext only after OpenSSL has
authenticated the whole archive and exited successfully.

Anyone holding the public certificate can produce an archive the private key
opens, so the encryption gives confidentiality, not authorship: keep archives
where only you can write them, and compare the SHA-256 that `oberth backup`
prints with the one `oberth restore` shows.

Create a dedicated recipient once, in an owner-only directory:

```sh
umask 077
mkdir -p ~/oberth-recovery && cd ~/oberth-recovery
openssl req -x509 -newkey rsa:4096 -nodes -days 3650 \
  -subj /CN=oberth-backup -keyout backup.key -out backup.pem
```

Keep `backup.key` (mode 0600; a restore refuses a key readable by group or
others) separately from the archives, and an independently protected copy of
it: without it no archive can be restored.

Plaintext never touches disk outside a private `0700` working directory that
is removed when the command returns. The root token is read from `BAO_TOKEN`
(or an echo-off prompt), removed from the process environment, and sent to
OpenBao only as the first stdin line of each `kubectl exec`; secret values
travel on stdin too. Neither ever appears in an argument list, which the API
server records in its exec audit log.

## Backup

```sh
read -rs BAO_TOKEN && export BAO_TOKEN
oberth backup --output ~/oberth-recovery/oberth-$(date -u +%Y%m%dT%H%M%SZ).cms \
  --recipient ~/oberth-recovery/backup.pem
```

Flags: `--context`, `--kubeconfig`, `--namespace` (default `oberth`),
`--openbao-namespace` (default `openbao`), `--argo-namespace` (default
`oberth-argo`), and `--dry-run`, which prints what would be collected without
reading OpenBao or writing a file. The output must not exist.

Backup changes nothing in the cluster. The database snapshot is taken by the
running server binary itself (`oberth backup database`, run through
`kubectl exec`): it copies the database and WAL privately, refuses if they
changed meanwhile, and verifies integrity, foreign keys, the schema identity
and the audit chain under the deployment's own `OBERTH_AUDIT_DOMAIN` before
streaming the result out. A server released before that subcommand existed
gets a stable copy instead: both files are streamed out and must hash
identically before and after, and the same verified snapshot is then built on
the workstation. A snapshot that races a write is retried up to three times.

The compatibility values recorded are the deployment's live
`OBERTH_SCHEMA_IDENTITY`, `OBERTH_AUDIT_DOMAIN` and `OBERTH_WITNESS_KEY_INFO`.
The archive is verified under them before it is written, so a restore never
has to guess the audit domain.

## Restore

Install Oberth on the target first (`oberth install --install-secretstore`),
in the same namespaces as the source, with a server release at least as new as
the one that wrote the backup. Then:

```sh
read -rs BAO_TOKEN && export BAO_TOKEN
oberth restore --input oberth-20261016T144942Z.cms \
  --private-key ~/oberth-recovery/backup.key --dry-run
oberth restore --input oberth-20261016T144942Z.cms \
  --private-key ~/oberth-recovery/backup.key
```

Before anything changes, the restore:

1. decrypts the archive and checks every member against the manifest;
2. validates the database offline with the store package: SQLite integrity,
   foreign keys, the exact schema version, the schema identity, and the full
   audit hash chain under the recorded audit domain, and checks the audit head
   against the manifest;
3. reads the target: the `oberth` Deployment and its volume claim, the Helm
   release, OpenBao (which must be unsealed), the target server's schema
   version, and the Argo Workflows whose runs the restored database does not
   know;
4. prints the plan — secrets, policies and roles that are new or replaced, the
   database swap, the Helm values to set, the workflows to delete — and asks
   for the target context name to be typed back (or takes `--yes`).
   `--dry-run` stops here.

It then:

1. writes every KV secret (JSON on stdin), policy and role, and reads each one
   back: secrets and policies must match exactly, roles field-for-field
   (deprecated aliases such as `policies` that mirror their `token_*`
   successor are not written twice, which OpenBao rejects). Any difference
   stops the restore before the server is touched;
2. scales `deploy/oberth` to 0 and waits for its pods to exit;
3. starts a short-lived pod with the server's image, identity and placement
   that mounts the server's PersistentVolumeClaim — no host paths, so any
   storage class works — uploads the database over stdin, re-checks its
   SHA-256 on the volume, moves `oberth.sqlite`, `-wal` and `-shm` into
   `/data/pre-restore-<UTC timestamp>/`, moves the restored files into place,
   and checks they are owned by the server user with mode 0600;
4. restores the `oberth-secret-access` ConfigMap. The server reconciles grants
   from it at startup, so it must be in place before the server starts;
5. deletes Argo Workflows whose `oberth.ci/run` label names a run the restored
   database does not know — they belong to the replaced database and would
   otherwise keep running and hold pipeline slots;
6. sets `compatibility.{schemaIdentity,auditDomain,witnessKeyInfo}` and, when
   recorded, `argo.releaseWIF` on the release with
   `helm upgrade --reuse-values` (published chart at the release's current
   version, or `--chart` for a local chart); skipped when nothing differs;
7. scales the server back up and waits for it to become ready.

If a step after the scale-down fails, the error says so; fix the cause and
rerun the restore (every step is idempotent), or scale the Deployment back up.
The previous database stays in its `pre-restore-*` directory until you remove
it.

`watchTunnel` settings are not reapplied: they pin the old server CA and are
re-established through the tunnel adoption flow
([watch-tunnel.md](watch-tunnel.md)). The restore lists the source
certificate's extra DNS names and IPs so the next install can reuse them.

### After a restore

1. Reconcile per-repo identities and policies from the restored grants, with
   your usual install flags:

   ```sh
   read -rs BAO_TOKEN && export BAO_TOKEN
   oberth install --install-secretstore --upgrade
   ```

   This also refreshes the pinned server CA on this machine.
2. Re-pin the new server CA on every other CLI and MCP client.
3. Uplinks come back with the database. Re-register any workstation whose key
   changed: `kubectl exec -i -n oberth deploy/oberth -- oberth uplink add - you@host < ~/.ssh/id_ed25519.pub`.
4. Re-run any trusted plan whose envelope was sealed under the old Transit key.
5. Release Workload Identity Federation trusts the source cluster's service
   account issuer; update the cloud provider's trust for the new cluster.
6. External audit witnesses (Rekor) may be ahead of the restored audit head;
   the server then refuses to start rather than rewrite history. Never delete
   witness records to make a restore pass; see
   [database-backup.md](database-backup.md).
