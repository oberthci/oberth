# Upgrading across a backup-and-replace schema boundary (v2 -> v3)

Store schema version 3 (introduced in v0.10.57 by step-level node budgets)
adds declared-size and durable resource-usage columns. `v1 -> v2` is the sole
ratified live migration; **every later schema evolution is backup-and-replace
by design**, so a live PVC can never be silently rewritten by a newly
deployed binary.

Upgrading a running pre-v0.10.57 (schema v2) install with a plain
`helm upgrade` therefore crash-loops the pod with:

```
store: database schema is incompatible with this binary: existing schema
version 2 requires backup-and-replace; live migration to version 3 is
disabled; see docs/upgrade-schema-v3.md
```

This is the refusal working as intended, not a corruption. Follow the
procedure below. Budget ~15 minutes of git-ingress and API downtime.

## What survives, what is archived

Survives the replace (namespace Secrets are untouched):

- `oberth-upstream-key` -- upstream deploy key; upstream re-registration
  reuses it, so nothing changes at your forge
- `oberth-ssh-host-key`, `oberth-tls` -- ingress trust material; uplink
  clients keep their pinned host key and TLS fingerprint

Archived with the old database (kept in the backup, absent from the new
store):

- the audit chain and its witness anchors -- a fresh store starts a new
  witness chain; if external anchoring (Rekor/TSA) is enabled, the server
  requires the explicit witness-chain-reset acknowledgment on first start
- run history and retained logs
- CI issue records
- uplink registrations and their bearer tokens -- **every uplink must be
  re-added and every MCP client updated with its new token**

## Procedure

1. **Quiesce.** Wait for in-flight runs to finish (or accept their loss),
   then scale to zero:

   ```bash
   kubectl scale -n oberth deploy/oberth --replicas=0
   ```

2. **Back up the store files** (the sqlite database plus its `-wal`/`-shm`
   siblings) from the `oberth-data` PVC. With the deployment stopped, mount
   the PVC read-only from a hardened throwaway pod.

   Download the backup locally and verify its integrity before proceeding:

   ```bash
   kubectl run oberth-backup -n oberth --restart=Never --rm -i \
     --image=busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0 \
     --overrides='{
       "spec": {
         "automountServiceAccountToken": false,
         "containers": [{
           "name": "oberth-backup",
           "image": "busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0",
           "command": ["tar", "cf", "-", "/data/oberth.sqlite", "/data/oberth.sqlite-wal", "/data/oberth.sqlite-shm"],
           "volumeMounts": [{"name": "data", "mountPath": "/data", "readOnly": true}],
           "securityContext": {
             "runAsNonRoot": true,
             "runAsUser": 65534,
             "allowPrivilegeEscalation": false,
             "readOnlyRootFilesystem": true,
             "capabilities": {"drop": ["ALL"]},
             "seccompProfile": {"type": "RuntimeDefault"}
           }
         }],
         "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "oberth-data"}}]
       }
     }' > oberth-store-backup.tar
   ```

   Verify the archive is intact and record its checksum:

   ```bash
   tar -tzf oberth-store-backup.tar
   sha256sum oberth-store-backup.tar
   ```

   A corrupt or empty archive means the backup failed -- do not proceed.

3. **Remove the live database files** from the PVC (same pattern,
   `rm -f /data/oberth.sqlite /data/oberth.sqlite-wal /data/oberth.sqlite-shm`).
   The v3 binary refuses to touch a v2 store byte-exactly; deleting the files
   is what authorizes the fresh v3 genesis.

4. **Deploy the new version.** `helm upgrade` to the target chart and scale
   back up. The pod starts with a fresh v3 store and stays `0/1 Running` --
   live, not ready -- until an upstream is registered, exactly like a first
   install. If external audit anchoring is enabled, acknowledge the
   witness-chain reset when prompted by the startup log.

5. **Re-register upstreams.** The surviving `oberth-upstream-key` Secret is
   reused; your forge-side deploy key registration stays valid:

   ```bash
   kubectl exec -n oberth deploy/oberth -- \
     oberth upstream add <name> <ssh-url>
   ```

6. **Re-add uplinks.** Each prints its bearer token once; update every MCP
   client and CI credential that held an old token:

   ```bash
   kubectl exec -i -n oberth deploy/oberth -- \
     oberth uplink add - <who>@<host> < ~/.ssh/id_ed25519.pub
   ```

7. **Verify the secret store path** (release burns depend on it):

   ```bash
   kubectl exec -n oberth deploy/oberth -- oberth secretstore verify
   ```

8. **Prove the loop.** Push a trivial feature branch through the ingress and
   watch its run go green before trusting the instance with a release tag.

## Later schema boundaries

Any future `existing schema version N requires backup-and-replace` refusal
follows this same procedure; only the version numbers in the error change.
