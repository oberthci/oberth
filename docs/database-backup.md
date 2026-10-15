# Private database recovery material

`cmd/oberth-store-backup` is a Linux maintenance utility built from reviewed
source. It is separate from the released `oberth` CLI and is not an MCP endpoint.
It does not install, upgrade, restore a live store, reset audit continuity, or
reconcile Kubernetes resources. Do not run it as an ordinary CI step against a
live database. A deployment-specific execution plan must bind its source and
binary, OpenSSL executable and libraries, exact PVC/PV/Pod identity, file mounts,
public protocol identifiers, and recipient certificate before execution.

The export reuses `snapshotCurrentDatabase` unchanged. That primitive copies the
main database and optional WAL to a private directory and refuses if subsequent
source hashes differ. It does not claim SQLite's Online Backup API locking
semantics. The private copy must pass the exact expected schema, integrity,
foreign-key, audit-chain and pending-publication checks before any archive bytes
reach encryption. It never opens the live database writable or runs migrations.
A schema-21 build can inspect schema 20 without upgrading it; `--expect-schema=20`
checks the private copy explicitly. Source changes or errors are refusals, not
permission to retry until a convenient snapshot succeeds.

The closed archive contains `oberth.sqlite`, optional `oberth.sqlite-wal`, and
`manifest.json`. The SHM index is derivative and is not archived. This is
**database recovery material, not a full PVC backup**. Git caches, workspaces,
logs, SSH identity Secrets, and external witness records remain separate.

## Execution and key custody

Run only in a reviewed retained-process sandbox with a private 256 MiB tmpfs at
`/tmp`, `MemorySwapMax=0`, an appropriate memory cap, no network, empty
capabilities, `NoNewPrivileges`, core dumps disabled, and positive process and
cgroup cleanup. Mount only the exact database and WAL read-only under a fixed
source directory. The utility independently checks tmpfs, its unified cgroup's
zero swap limit, no-new-privileges and empty capability sets. The launcher must
additionally prove mount isolation and resource limits. Inputs are bounded to
128 MiB; oversized databases require a new reviewed bound, not a hidden change.

Use a dedicated RSA recipient of at least 3072 bits. Generate its private key
only inside an owner-only recovery directory, with umask 077 and an exclusive
0600 file; retain the public certificate and its SHA-256 separately. Never print
key bytes or put them in command arguments or environment variables. The export
worker receives only the public certificate. A separate verification worker may
read the exact private key through a read-only mount. Do not search for or reuse
unrelated credentials. Same-host separate key storage does not protect against
full host compromise; retain an independently protected recovery copy according
to the operator's approved retention policy before deleting either local copy.

The utility uses `/usr/bin/openssl cms` with AES-256-GCM AuthEnvelopedData and
RSA-OAEP-SHA256. It checks a required executable digest and an exact public
certificate digest. No custom encryption format is introduced. Plaintext flows
through a private pipe; only the authenticated encrypted archive is retained in
an exclusive 0600 output in an owner-only directory. File and directory fsyncs
precede success. Failed newly created outputs are removed; an occupied path is
never overwritten or removed. Fixed errors do not include paths, SQL, rows,
OpenSSL diagnostics, or key material.

Build the utility from the exact reviewed commit with the repository's pinned
Go version. Within the qualified sandbox, its command shape is:

```text
oberth-store-backup --database /run/backup-source/oberth.sqlite \
  --expect-schema 20 --schema-identity ORIGINAL --audit-domain ORIGINAL \
  --recipient /run/backup-recipient.pem --recipient-sha256 PIN \
  --openssl-sha256 PIN --output /run/backup-output/store.cms
```

Use original public protocol values from the existing deployment. Do not infer
or replace them from database rows. The same fixed inputs and tool pin are used
for a separate private verification:

```text
oberth-store-backup verify --input /run/backup-input/store.cms \
  --ciphertext-sha256 PIN --private-key /run/backup-key/recipient.key \
  --openssl-sha256 PIN --expect-schema 20 \
  --schema-identity ORIGINAL --audit-domain ORIGINAL
```

OpenSSL can emit provisional plaintext before rejecting authentication. The
verification command keeps it in private tmpfs, waits for a successful child
exit before parsing anything, checks the canonical closed archive inventory and
hashes, and repeats read-only schema and audit validation in a new directory.
All plaintext is removed on exit. It never restores into the live PVC. Retained
outer-process containment must also clean up interrupted invocations.

## Forward migration and restoration limits

A forward upgrade from schema 20 to 21 adds the nullable termination diagnostic
column and retains the schema ledger and audit identities. It is not the
historical destructive v2-to-v3 replacement procedure. An older binary must
refuse a schema-21 store. Do not use `helm rollback` as a database rollback.

Successful decryption and private validation establish recovery bytes at the
recorded audit head. They do not authorize a later live restore. External audit
intents or witnesses may advance beyond that head; restoring older bytes can
then be correctly refused. Never delete or reset witness records to make a
restore pass. A future restore requires a separate reviewed plan that preserves
identity resources and reconciles the exact current external continuity state.
If that compatibility cannot be proved, retain the encrypted backup and repair
forward. Do not claim the backup alone guarantees rollback.

Keep the encrypted archive and its dedicated private key until the forward
upgrade and required protected positive/negative checks are complete and an
explicit recovery-retention decision is made. No automatic campaign cleanup
removes them. A public receipt contains only the schema, verified audit head,
verification booleans, ciphertext digest/size and public recipient fingerprint.
It contains no database rows or private key material.

The verifier accepts only definite-length DER AuthEnvelopedData with data content,
AES-256-GCM, a 12-byte nonce, an explicit 16-byte authentication tag length, a
single recipient, and no optional attributes. It rejects unauthenticated CBC
EnvelopedData before decryption; OpenSSL still performs all cryptography.
Receipts require successful command exit: synchronous absolute-deadline checks
run before and after receipt emission. A blocked write cannot be recalled, so
the retained outer deadline and exit verdict remain required.

Mandatory sandbox qualification builds the command test binary with
`-tags=backup_synthetic` and runs `TestEncryptedBackupActualCMS` in the reviewed
private no-swap sandbox. Selecting that fixture without its sandbox configuration
is a fatal error. Default argument, DER-format and deadline tests run normally.

The committed mandatory integration entrypoint is
`scripts/qualify_database_backup.sh`. Build the fixture using the repository's
qualified Go toolchain with:

```sh
go test -p 1 -race -tags=backup_synthetic -c -o /reviewed/bundle/backup.test ./cmd/oberth-store-backup
```

Bind the exact binary SHA256, source inventory, OpenSSL and shared-library hashes
before entering the reviewed sandbox. Inside it invoke the entrypoint with the
absolute binary path and its exact lowercase SHA256. The entrypoint fixes the
one test selector, count, 90-second test deadline and minimal environment; it
cannot replace the retained host launcher. The launcher must provide private
256MiB tmpfs, 512MiB memory, swap zero, one CPU, empty capabilities, no-new-privileges,
private networking restricted to AF_UNIX, no live database mount, and a bounded
120-second workload with retained pidfd/cgroup/launcher joins. Its independently
checked result must record every named test, including actual ENOSPC through the
production ciphertext writer and CBC generic-decrypt-positive/production-refusal.
Keep any failed attempt and bind successful evidence to the exact source and
binary; a source-only gate is not a substitute for this actual qualification.
