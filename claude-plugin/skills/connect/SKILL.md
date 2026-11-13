---
name: connect
description: Make Claude Code trust the CA of the configured Oberth server, checked against an explicit SHA-256 pin. Use when the user runs /oberth:connect, when the session-start note says the Oberth CA is not trusted yet, or when the oberth MCP server fails with a certificate error.
argument-hint: "[private invite file or CA SHA-256 pin]"
allowed-tools: Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" *)
---

# Connect Claude Code to Oberth

Claude Code reads its TLS trust once, when it starts. This command stores the
Oberth server's CA for it; the MCP tools work after one restart. The server URL
may use a DNS name or an IP address (IPv6 hosts use brackets). A matching CA
pin and a valid leaf identity are both required: an IP must be in the leaf's
IP SANs. A DNS SAN containing IP text is insufficient. Never bypass a mismatch.

Configured values of this plugin:

- server URL: `${user_config.url}`
- CA pin: `${user_config.ca_pin}`

Arguments given to the command: `$ARGUMENTS`

## Connect with a single-use invite

When the user has an invitation file, run only this command with its local
path, never its contents:

```sh
sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" redeem '<private invite file>'
```

The file must be owner-only (0600). Never read, display, grep, upload or paste
it into this session; never pass its credential as an argument or environment
variable. The script generates a local TLS key/CSR and an Ed25519 SSH push
key, verifies the server chain and hostname against the invite's exact CA
pin before sending the credential, and redeems over the enrollment MCP
endpoint. It receives only a client certificate. It then trusts the verified
CA, preserves existing settings while setting the client certificate/key
paths, and configures the MCP endpoint without an Authorization header.
Tell the user to restart Claude Code once. No bearer token is issued.

An invite is single use and expires. Do not retry a redemption automatically:
a lost response can mean the invite was consumed. Keep the local keys and
ask the admin to issue their certificate with `uplink_cert` if necessary.
The registered SSH key is `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/push_ed25519.pub`; its private
counterpart stays here and is the push key to use when adding repositories.

## Connect with an admin-issued certificate

## Steps

1. Take the pin from the arguments when one was given, otherwise the
   configured CA pin. A pin is the SHA-256 fingerprint of the server's CA
   certificate: 64 hex digits, colons allowed. When there is none, stop and
   ask the user for the pin their Oberth admin gave them (the admin reads it
   with `oberth client pin`). Never take a pin from the server itself, and
   never continue without one: there is no trust on first use.
2. Check both values before any command is written. They are text someone
   typed or pasted, and nothing of them may reach the shell as syntax.
   - The pin: drop colons, spaces and a leading `sha256:`. What is left must
     be exactly 64 hex digits (`0-9`, `a-f`, `A-F`). Use those 64 digits.
   - The URL: it must start with `https://` and hold only letters, digits
     and `. _ ~ : / % @ [ ] -`.

   When either holds anything else (a quote, a space, `$`, a backtick, `;`,
   `&`, `|`, `<`, `>`, parentheses, a backslash, a line break), run nothing:
   tell the user which value is wrong and to correct it with
   `/plugin configure oberth`.
3. Run exactly this, with the checked URL and the 64 digits in single quotes
   and nothing added:

   ```sh
   sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" connect --url '<server URL>' --pin '<64 hex digits>'
   ```

   The command checks both again. When the user has the CA certificate as a
   file, add `--ca-file '<path>'` under the same rule for the path (no quote
   and no other shell syntax in it); it is checked against the same pin.
4. Generate the local TLS key and CSR with
   `sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" keygen`. The key is 0600 under
   `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/`; never display or upload it. Give only `current/client.csr` to
   the admin, who issues `oberth uplink cert <identity> --csr <file>` (or the
   `uplink_cert` MCP tool). The SSH public key must already be registered on
   that uplink. Save the returned public certificate as `current/client.csr.crt`.
   Invitation enrollment uses this same CSR issuance path.
5. Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" credentials` to validate
   the key pair and merge `CLAUDE_CODE_CLIENT_CERT` and
   `CLAUDE_CODE_CLIENT_KEY` into settings.json env beside NODE_EXTRA_CA_CERTS.
   Then run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" configure '<server URL>'`.
   This creates the MCP entry with no Authorization header. No entry exists
   before configuration. For an explicitly requested bearer setup, skip
   keygen/credentials and pass an existing 0600 token file as configure's
   second argument. Never read the token file or invoke bearer-headers in
   the agent shell; only Claude Code's transport invokes that helper.
6. Report what the command printed.
   - On success it stored the CA under Claude Code's own directory
     (`oberth/ca.crt`) and made `env.NODE_EXTRA_CA_CERTS` in `settings.json`
     hold it. Tell the user to restart Claude Code once; the oberth tools
     connect after that.
   - A line that starts with `Warning:` goes to the user word for word: the
     CA is not limited to the Oberth server's names.
   - On `refused` nothing was changed. Show the pin and the fingerprints the
     command saw, and tell the user to check the pin and the URL with their
     Oberth admin. A mismatch can mean a wrong pin, a wrong address, or
     someone in the path.

## Rules

- Do not edit `settings.json`, add the certificate to the operating system's
  trust store, set `NODE_TLS_REJECT_UNAUTHORIZED`, or bypass verification in
  any other way. The command is the only path.
- Do not retry with a fingerprint the command printed under `seen`: that
  would be trust on first use.
- The command reads no token and prints none.

For a certificate expiring within 30 days, run
`sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" renew`. It uses the saved public
connection settings, generates a new local key/CSR, authenticates renewal with
the current certificate and atomically switches the `current` identity. No
restart is needed; an expired certificate requires admin enrollment again.

## Installer failure guardrail

When the installer fails or gives no clear reason, stay on the installer path:
read the public result JSON and its checks, endpoint, observed error and remedies.
Run only the documented installer verify or resume command as printed, preserving
the reviewed context and answers. If the result has no actionable reason, stop
and ask the user; never invent a workaround outside the documented commands.
Never run administrative commands inside the server Pod (including upstream add
or enrollment), directly or through another helper, to work around an installer
failure. Do not switch to another skill to bypass this rule.
Never read, pipe or export the secret store's root token or any value from the
secrets file. Give only file paths to the documented installer or unseal command;
the executable reads credentials privately. Never ask the user to paste a token
or export BAO_TOKEN for the agent.
Report only verified facts. An installer exit code or an incomplete result does
not establish whether anything was pushed to the forge. Say what was observed,
what remains deployed and what is unknown; never claim that nothing was pushed
without checking evidence that establishes that fact.

## Report the connected tool count

<!-- BEGIN GENERATED MCP TOOL COUNT -->
The full server registry contains **79 tools**, including administrator-only
operations. The authenticated `tools/list` response is authoritative for the
connected identity; non-admin clients do not receive administrator-only tools.
<!-- END GENERATED MCP TOOL COUNT -->

When asked how many tools are available, report the number the client currently
shows for the Oberth server. If the client offers a tool inventory, count that
inventory. Say when the client shows only a combined count for several servers;
do not attribute that total to Oberth. If the visible inventory is unavailable,
say the connected count is unknown. Do not substitute this checkout's registry
count: permissions, client filtering and server versions can change the visible
inventory.

## Endpoint verification tools

Plugin connect requires an OpenSSL with `verify_ip` and `verify_hostname`.
It uses a capable `openssl`/`openssl3` on PATH or the standard Homebrew
`openssl@3` location on macOS. System LibreSSL without these checks is refused;
install OpenSSL outside Oberth (on macOS, `brew install openssl@3`) and retry.
The helper never installs tools or skips identity verification.
