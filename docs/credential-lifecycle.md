# Remote credential and grant lifecycle

An unscoped administrator can use `uplink_list {}` to inspect identity,
fingerprint, scope, admin status, creation time, last seen and revocation time.
All five new lifecycle tools require an unscoped admin; ordinary and scoped
uplinks are refused. Tokens and private keys are never included.

`uplink_revoke {"identity":"agent@host","fingerprint":"SHA256:..."}` revokes
both SSH and bearer authentication for subsequent requests. Specify a fingerprint
when the identity has several keys. The last active unscoped administrator
credential is protected. Revoked registrations remain visible in the list.

Rotation uses the existing in-pod `oberth uplink add` issuance path to provision
and deliver the replacement token first. Then call:

```
uplink_rotate {"identity":"agent@host","fingerprint":"SHA256:old","replacement_fingerprint":"SHA256:new"}
```

The replacement must be active and have the same identity, admin status and
scope. The server validates that condition and revokes the old credential in
one audited transaction. A failed check leaves the old credential usable.
MCP never issues or accepts a plaintext bearer or private key.

`access_list {"pending":true}` tracks both pipeline files at the latest admitted
commit. Removed paths disappear on the next successful admission snapshot;
invalid or unreadable files preserve existing observations. For remaining old
observations, an admin can use `access_dismiss {"repo":"forge/org/repo",
"step":"publish","secret":"oberth/data/old"}`. This removes the observation,
not any grant; admission may observe it again. Copy the exact repository key
from the pending list. Unscoped admins can also dismiss legacy bare-name or
orphaned observations.
Admission clears old bare-name observations only when the catalog name is
unambiguous.

`kubevirt_grant_list {}` returns active grants across all repositories. `repo_list` and `/api/repos` also include optional `KubeVirtGrant`
metadata, including a revoked grant when present. Scoped admins cannot see or
change other upstreams. No new cluster permissions or schema migrations are
needed.
