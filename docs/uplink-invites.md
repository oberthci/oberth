# Uplink invitations

An unscoped admin can invite a new identity to an existing Oberth server.
The invitee generates a TLS key/CSR and an SSH push key locally. Enrollment
returns a client certificate bound to the new uplink; it issues no bearer
token. Both private keys stay on the invitee's machine.

## Create and transfer

From an already authenticated admin client:

```sh
oberth uplink invite agent@example.test --scope github --expires 24h \
  --out "$HOME/oberth-invite.json"
```

`--out` is required and must be a new file outside a git worktree. The command
writes it with mode 0600 and prints only its path and expiry. Transfer this
file through a private channel. Never paste, display, read or grep its
contents in an agent session. The file contains a random 32-byte one-time
credential, the HTTPS MCP URL, the server CA's DER SHA-256 pin, the identity,
scope and expiry. The `secret` field is unpadded base64url of the 32 random
bytes; `secret_digest` is lowercase hex SHA-256 of those decoded bytes.
The default lifetime is 24 hours; the maximum is 168 hours.
The invitation grants regular uplink access, with the selected upstream
scope; it cannot grant admin rights or replace an existing identity/key.

The `uplink_invite` admin MCP tool accepts `identity`, `server_url`,
`secret_digest`, optional `scope`, and optional `expires`. The credential
must be generated locally with cryptographic randomness. Only its SHA-256
digest is sent to that tool; its response contains public invitation
metadata. An agent should use the CLI to create the handoff file so neither
the one-time credential nor an authentication token enters the transcript.

## Connect from the plugin

Give `/oberth:connect` the local path of the received 0600 invitation file.
It invokes the plugin's `trust redeem` script. When the CLI is absent the
script uses the plugin's verified fetcher. The script creates its local TLS
identity under `~/.claude/oberth/current/` and an Ed25519 SSH key at
`~/.claude/oberth/push_ed25519`; it registers only the public SSH key.

The client checks the server's certificate chain, hostname, validity and
name constraints against the exact invitation CA pin before sending the
credential. It follows no redirects. After redemption, the script stores
the authenticated public server CA, preserves existing Claude Code settings
while adding the certificate/key paths, and configures the `/mcp` entry
without an Authorization header. Restart Claude Code once for initial CA
trust. The SSH private key remains available locally for repository pushes.

For an existing locally generated CSR and SSH key, the same operation is:

```sh
oberth client redeem --invite-file "$HOME/oberth-invite.json" \
  --csr "$HOME/.claude/oberth/current/client.csr" \
  --ssh-public-key "$HOME/.claude/oberth/push_ed25519.pub" \
  --certificate-out "$HOME/.claude/oberth/current/client.csr.crt"
```

The certificate output and its `.ca.crt` and `.url` companions must all be new.
They are reserved before sending the request. Refused requests remove only
those newly reserved public output files. A successful redemption followed
by a disk failure keeps its output files and explains how to recover.
No command reads or transmits the local private keys.

## Enrollment boundary and recovery

`POST /mcp/enroll` implements MCP initialization, ping, tool discovery and
exactly one tool: `uplink_redeem`. It is separate from the authenticated
`/mcp` endpoint and accepts no bearer headers or existing MCP session IDs.
Request sizes and operation time are bounded, failed credentials use the
server's authentication failure limiter, and responses prohibit caching.
No other API or MCP tool is available through enrollment.

The database holds only the credential digest. Redemption checks that the
invite is unused, unexpired and bound to the supplied URL and current server
CA pin. Consumption, creation of the certificate-only uplink, SSH fingerprint,
scope, certificate SPKI binding and audit receipts commit in one transaction.
Malformed CSRs, wrong pins, signing failures or audit failures leave the
invite unconsumed. Concurrent redemption has one winner. Both creation and
redemption are audited without the credential or CSR private material.

Do not retry automatically after an uncertain network result: a lost reply
can mean that the invite was spent. Keep the local keys and ask the admin
for `oberth uplink cert <identity> --csr <file>` or the `uplink_cert` MCP tool.
If the CA pin is wrong, obtain a new invitation from the admin; never replace
it with a pin learned from the server. If the server CA changes, old invitations must be replaced. Removing or revoking the uplink immediately
removes the certificate's access, including on an existing TLS connection.
