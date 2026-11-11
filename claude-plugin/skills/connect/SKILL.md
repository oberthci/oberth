---
name: connect
description: Make Claude Code trust the CA of the configured Oberth server, checked against an explicit SHA-256 pin. Use when the user runs /oberth:connect, when the session-start note says the Oberth CA is not trusted yet, or when the oberth MCP server fails with a certificate error.
argument-hint: "[CA SHA-256 pin]"
---

# Connect Claude Code to Oberth

Claude Code reads its TLS trust once, when it starts. This command stores the
Oberth server's CA for it; the MCP tools work after one restart.

Configured values of this plugin:

- server URL: `${user_config.url}`
- CA pin: `${user_config.ca_pin}`

Arguments given to the command: `$ARGUMENTS`

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
4. Report what the command printed.
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
