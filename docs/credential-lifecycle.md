# Remote credential and grant lifecycle

An unscoped administrator can use `uplink_list {}` to inspect identity,
fingerprint, scope, admin status, creation time, last seen and revocation time.
Credentials awaiting activation have `pending: true` and no revocation time.
All five new lifecycle tools require an unscoped admin; ordinary and scoped
uplinks are refused. Tokens and private keys are never included.

`uplink_revoke {"identity":"agent@host","fingerprint":"SHA256:..."}` revokes
both SSH and bearer authentication for subsequent requests. Specify a fingerprint
when the identity has several keys. The last active unscoped administrator
credential is protected. Revoked registrations remain visible in the list.

Rotation uses the authenticated `oberth uplink add` API to provision
and deliver the replacement token first. Then call:

```
uplink_rotate {"identity":"agent@host","fingerprint":"SHA256:old","replacement_fingerprint":"SHA256:new"}
```

The replacement must be active and have the same identity, admin status and
scope. The server validates that condition and revokes the old credential in
one audited transaction. A failed check leaves the old credential usable.
Enrollment uses the authenticated `oberth uplink add` API with a public SSH
key and, for client certificates, a CSR. The client keeps its private key.
Bearer enrollment returns the new bearer once over the authenticated connection;
store it privately. The `upstream_provide_key` administrator operation accepts a
private upstream key for import into OpenBao. Inventory and revocation responses
never return existing bearer or private-key material.

Remote revocation retains the registration and reserves its fingerprint.
Use a new SSH key for a replacement and provision it before rotation; rotation
needs the old registration to validate identity, privilege and scope. Revoked
registrations remain visible as history and their fingerprints stay reserved.
There is no in-Pod removal step in this remote lifecycle.

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

MCP `repo_list` returns compact repository and latest-run records, without
grant metadata.

These tools use existing database and audit authority only. They require no
new chart values or Kubernetes RBAC.

Open dashboard live-log streams revalidate the bearer credential every 30 seconds.
A revoked or rotated credential, unavailable authentication, or changed identity,
admin flag or scope closes the stream with an `auth_error` SSE event directing
the client to reconnect with a current token. Authentication checks have a
five-second deadline. Other HTTP requests authenticate on every request.

The private in-pod administrative socket requires Linux peer credential checks.
On other platforms `serve` refuses to start that socket. Local developer tools
and remote clients remain usable without running the daemon.
