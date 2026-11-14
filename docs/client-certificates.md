# Client certificates

Oberth's API and MCP listener verifies optional client certificates against a
**dedicated client CA**, an install identity in OpenBao separate from the server
CA. A verified certificate must also have its SHA-256 SPKI pin registered on an
active uplink. Removing or revoking that uplink denies its certificates
immediately, including revalidation of MCP and log streams. Expiry is checked on
every request, including reused TLS connections. Audit details record
`auth_method: cert` or `bearer`.

Bearer credentials remain available for automation and metrics. Supplying both
requires the same uplink, admin status and scopes. An empty `Authorization:
Bearer ` is ignored only when a valid certificate authenticates. An unknown pin
never falls back to a bearer.

Clients may connect using DNS names or IPv4/IPv6 literals. A DNS choice must
match the server certificate's DNS identity; an IP requires a matching IP SAN, even
when the CA pin matches. IPv6 URL hosts use brackets. The installer adds the
chosen client host to the leaf SANs. CA generation and existing constraints
are unchanged; DNS constraints do not constrain IP identities. See
[single-host TLS names](single-host-tls.md).

## Enrollment

Generate an EC P-256 or Ed25519 private key locally, mode 0600, and send only its
PEM CSR. The issuer ignores the CSR's requested subject and extensions: the CN
and `oberth://uplink/<identity>` URI come from the registered uplink, with only
clientAuth EKU. The default lifetime is 2160h (90 days); `--ttl` / MCP `ttl` can
select 1h through 8760h. A request exceeding the CA's own lifetime is refused.

An authenticated admin runs:

```sh
oberth uplink cert agent@host --csr client.csr > client.crt
```

Use `--fingerprint` when the identity has several SSH keys. The admin MCP tool is `uplink_cert` with
`identity`, `csr`, and optional `fingerprint` and `ttl`. Both return only the
public certificate. The shared service function `IssueClientCert` commits its
pin before returning; invite redemption can call it after authorizing the
invite. The invite command is a separate feature.

For a new uplink, `oberth uplink add --csr client.csr --admin <SSH-public-key>
<identity>` creates a certificate-only lifecycle record. It generates **no
bearer secret**. `--bearer` explicitly requests a bearer as well. Certificate-only
records are excluded from all bearer digest lookups.

## Install and Claude Code

`oberth install --admin-ssh-public-key <public-file> --admin-csr <CSR-file>`
creates the first admin and writes the public certificate to `<CSR-file>.crt`.
These settings also work as `AdminSSHPublicKeyPath`, `AdminCSRPath`, and
`AdminBearer` in the answers file. `--admin-bearer` opts into an additional
bearer saved through the usual secrets-file ceremony. The default creates none.

The plugin's deterministic commands are:

```sh
sh "$CLAUDE_PLUGIN_ROOT/scripts/trust" keygen
sh "$CLAUDE_PLUGIN_ROOT/scripts/trust" connect --url '<https MCP URL>' --pin '<CA SHA-256>'
sh "$CLAUDE_PLUGIN_ROOT/scripts/trust" credentials
sh "$CLAUDE_PLUGIN_ROOT/scripts/trust" configure '<https MCP URL>'
```

Keygen creates a private version directory under `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/` and the
`current` symlink; the public CSR is `current/client.csr`. Supply this CSR to the
installer or administrator before `credentials`. The issued certificate is
`current/client.csr.crt`. Existing key material is never overwritten.

`credentials` checks the pair and merges `CLAUDE_CODE_CLIENT_CERT` and
`CLAUDE_CODE_CLIENT_KEY` into settings.json `env`, preserving existing settings
and `NODE_EXTRA_CA_CERTS`. After the first restart Claude Code uses mTLS. Its
SessionStart hook reports a missing or expiring certificate. The plugin has no
MCP entry until configure succeeds, so installing it before a server exists does
not create a failed empty endpoint.

Certificate mode has no Authorization header. An explicitly selected bearer
setup passes a 0600 token file as configure's second argument; only the HTTP
transport invokes `bearer-headers`, never an agent shell call. `oberth client
setup --client-cert <file> --client-key <file>` also merges the client settings;
`--token-file` selects a private bearer file and header helper instead. Explicit
bearer setup removes the previous client certificate/key settings while keeping
other environment settings.

## CLI and renewal

Every remote REST/MCP command accepts `--client-cert` and `--client-key` beside
`--ca-file` and `--url`. Environment equivalents are `OBERTH_CLIENT_CERT` and
`OBERTH_CLIENT_KEY`; client env files can name both, or use `client.crt` and
`client.key` beside the saved server configuration. Saved credentials never
follow a different server URL. Explicit certificate selection does not
implicitly load a saved token. To deliberately send both, add `--token-file`.

Before expiry, run `oberth client renew` with the same connection flags.
The plugin's `scripts/trust renew` supplies these flags from its saved URL
and Claude Code settings. The
`client_renew` MCP tool requires current certificate authentication, accepts a
new local CSR and optional `ttl`, and can renew only its own uplink. It adds the
new pin before returning; the old pin remains valid until expiry. The CLI
creates a new P-256 key and publishes the complete validated pair by atomically
switching the shared `current` symlink. Claude Code and CLI handshakes read the
same configured paths; no restart is required. Old version directories are
retained for operator-controlled cleanup. For unmanaged files, pass
`--output-dir <new-directory>` and configure the new paths explicitly.

Expired certificates need administrator enrollment. A restored database still
contains pins, but its original client CA is an install identity; re-enroll
clients against the new install's CA.

Plugin connect requires an OpenSSL with `verify_ip` and `verify_hostname`.
It uses a capable `openssl`/`openssl3` on PATH or the standard Homebrew
`openssl@3` location on macOS. System LibreSSL without these checks is refused;
install OpenSSL outside Oberth (on macOS, `brew install openssl@3`) and retry.
The helper never installs tools or skips identity verification.
