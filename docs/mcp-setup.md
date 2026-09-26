# MCP setup

Connect an AI agent (Claude Code or any MCP client) to Oberth.

## Prerequisites

- Oberth is deployed and has at least one upstream registered
- `kubectl` access to the Oberth namespace

## 1. Create an uplink

Run inside the Oberth pod:

```bash
kubectl exec -i -n oberth deploy/oberth -- \
  oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub
```

Replace `operator@host` with a descriptive identity for this uplink.

The command prints three lines exactly once. Save both values:

```
TLS certificate fingerprint: SHA256:abc123...
Uplink token for operator@host (shown once):
oberth_xxxxxxxxxxxxxxxx
```

The token is never stored or recoverable. If lost, create a new uplink.

## 2. Configure Claude Code

`oberth install` offers to write this for you on an interactive install, right
after it registers the uplink, into `~/.config/oberth/mcp.json`. What follows is
the same thing by hand, and the explanation of why it is shaped the way it is.

### Keep the token out of the file

The obvious configuration puts the bearer token in a literal header, and then a
credential lives in a file for as long as the file does. `headersHelper` names a
command that prints the headers instead, run fresh on every connection:

```json
{
  "mcpServers": {
    "oberth": {
      "type": "http",
      "url": "https://oberth:30443/mcp",
      "headersHelper": "printf '{\"Authorization\":\"Bearer %s\"}' \"$(security find-generic-password -s oberth-token -w)\""
    }
  }
}
```

That is the same command `OBERTH_TOKEN_COMMAND` names for the CLI, so one
secret source serves both clients and neither configuration file holds a
credential. Substitute your own: `secret-tool lookup service oberth`,
`pass show oberth/token`, `op read op://vault/oberth/credential`.

Three things worth knowing before relying on it. The command runs in a shell
with a ten second budget and its result is not cached, so a keychain read is
comfortable and an interactive unlock may not be. A project- or local-scope
server runs the helper only once the folder is trusted. And on a 401 or 403 the
helper is re-run once and the call retried, so a rotated uplink token recovers
without editing anything.

`headersHelper` is Claude Code's rather than part of MCP. A client that does not
implement it needs the literal header below.

### With a literal token

Add to `.claude/settings.local.json` (user-scoped, never committed) or user-level
config. **Do not** place bearer tokens in `.claude/settings.json` — that file
is typically checked into the repository.

### Via the HTTPS NodePort

```json
{
  "mcpServers": {
    "oberth": {
      "type": "url",
      "url": "https://<node-address>:30443/mcp",
      "headers": {
        "Authorization": "Bearer oberth_xxxxxxxxxxxxxxxx"
      }
    }
  }
}
```

This is the shipped access path — Oberth exposes fixed NodePorts and carries
no tunnel subsystem. If you front the NodePort with your own TLS-terminating
proxy (an ingress, a Cloudflare Tunnel you operate), point the URL at that
hostname instead; a publicly trusted certificate there removes the
client-side trust step below.

Direct access uses a self-signed certificate, and the chart issues it for
in-cluster names only. Reaching the server on any other address is a hostname
mismatch, which no amount of trust configuration repairs, so name the address
first: `oberth install --tls-extra-dns-name <name>` (a macOS kind install adds
`localhost` and `127.0.0.1` by itself). Only then is trusting the certificate
the remaining problem.

Export it and add it to the system trust store:

```bash
kubectl get secret -n oberth oberth-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 --decode > oberth-tls.crt

# Verify fingerprint matches uplink add output
openssl x509 -in oberth-tls.crt -outform DER | \
  openssl dgst -sha256 -binary | openssl base64 -A | \
  sed 's/^/SHA256:/' | tr -d '='

# Linux (Debian/Ubuntu)
sudo cp oberth-tls.crt /usr/local/share/ca-certificates/oberth.crt
sudo update-ca-certificates

# macOS
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain oberth-tls.crt
```

## 3. Verify the connection

After restarting Claude Code, the MCP server appears in the tool list. Verify
by asking Claude Code to call any Oberth tool, or test manually:

```bash
curl --fail-with-body --silent \
  --cacert oberth-tls.crt \
  --resolve "oberth:30443:<node-address>" \
  -H "Authorization: Bearer oberth_xxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  "https://oberth:30443/mcp"
```

A successful response lists the server's tools; `oberth version` and this
server's own `status` tool are the authorities on how many it should be, since
the count moves with the release. Behind an operator-run TLS-terminating
proxy with a publicly trusted certificate, drop `--cacert` and `--resolve` and
call the proxy hostname directly.

## MCP tools

| Tool | Description |
|------|-------------|
| `status` | CI status for a SHA or branch, including the failed step |
| `logs` | One named step's log output for a SHA, optionally filtered |
| `run_get` | One exact durable run and its named step results by run ID |
| `run_logs` | One exact burn/step log by durable run ID, optionally filtered |
| `artifacts` | The files a run kept, with size and modification time |
| `artifact_get` | One artifact's contents by run ID and name, optionally filtered |
| `wait` | Long-poll until a SHA reaches a terminal state |
| `sync` | Park a WIP branch upstream without a green gate (not completion evidence) |
| `promote` | Green-gate a SHA, merge with target branch, push without force |
| `promote_status` | Wait for a promotion record to become terminal |
| `issue_create` | Create a workspace-global manual issue |
| `issue_get` | Get an issue by ID |
| `issue_update` | Update an issue title and body |
| `issue_close` | Close an issue |
| `issue_delete` | Delete an accidentally created manual issue |
| `issue_list` | List issue IDs and states (paginated, 50 per page) |
| `issue_lock` | Acquire or renew a five-minute caller-owned issue lock |
| `access_list` | List secret access grants for a repository |
| `access_allow` | Grant a step access to a secret path (admin-only) |
| `access_revoke` | Revoke a step's access to a secret path (admin-only) |

### Filtering log output

`logs`, `run_logs` and `artifact_get` accept five optional parameters. Filtering happens on the
server, so a narrowed read never sends the whole step over the wire.

| Parameter | Meaning |
|------|-------------|
| `pattern` | RE2 expression; only matching lines are returned |
| `context` | Lines of context on each side of a match (max 50) |
| `offset` | First line to return, 0-based. Pages over matches when `pattern` is set, over raw lines otherwise |
| `limit` | Maximum lines returned. Omit for no line cap |
| `tail` | Take from the end of the step rather than the start |

`pattern` matches against the line with its `[burn/step]` prefix removed, so `^`
and `$` anchor to the output a step actually produced. The returned bytes keep
the prefix.

Every response carries counts so a narrowed read is never mistaken for a
complete one:

| Field | Meaning |
|------|-------------|
| `total_lines` | Lines in the step before any filtering |
| `matched_lines` | Lines matching `pattern`; equals `total_lines` without one |
| `returned_lines` | Lines actually in this response |
| `truncated` | Whether a limit or the response ceiling withheld anything |
| `bytes` | True size of the full step range |
| `line_numbers` | Original position of each returned line |

A step above the 4 MiB response ceiling returns a truncated result with
`truncated: true` rather than failing. Prefer a pattern to retrieving a whole
step: a single step can exceed a model's context window.

| `repo_list` | List registered repositories with upstream and probe state |
| `repo_remove` | Remove a repository mapping and its Git cache (admin-only) |
| `run_list` | List recent runs with optional repo/ref filter (bounded page) |
| `system_status` | System health: database, upstreams, cluster, audit, version |
| `secretstore_verify` | Admin-only real secret-store preflight using configured server or per-repo release/CI identity; paths/key counts only by default |

## JSON API

Authenticated `GET` endpoints serve the same state as a web dashboard:

| Endpoint | Content |
|----------|---------|
| `/api/runs` | All CI runs |
| `/api/repos` | Registered repositories |
| `/api/issues` | All issues |
| `/api/status` | System health summary |

Unauthenticated: `/healthz` (liveness) and `/readyz` (dependency readiness).

## Notes

- The MCP endpoint accepts `POST /mcp` with JSON-RPC 2.0 (protocol version
  `2025-03-26`).
- Bearer tokens are bound 1:1 to an uplink SSH public-key fingerprint and
  identity.
- Run selectors (`ref`, `sha`) are resolved across all repositories; pass
  `repo` only when a short SHA or branch name is ambiguous.
- `sync` parks a branch upstream for visibility. It is not integration or
  completion evidence.
- `promote` requires explicit integration authority, an exact green SHA, and a
  named target branch. It never force-pushes.

### Secret-store release preflight

An admin uplink can call `secretstore_verify` instead of entering the server pod:

```json
{"release_tier":true,"repo":"github/oberthci/oberth","paths":["oberth/upstream/oberthci/oberth/signing"],"timeout":45}
```

Use the exact upstream/org/repo identity and declared paths for the release.
`release_tier` selects the configured pipeline trust; `tier` defaults to
`release` and accepts `ci` only with a per-repo identity. Omitting
`release_tier` checks the server identity; that mode supports `keys` and
`expect` (for example `["signing/cosign.key,cosign.password"]`). Empty `paths`
uses the server's configured verification defaults. At most 32 paths and
expectations are accepted; timeout is bounded to 120 seconds including
TokenRequest, login and reads. No caller-selected connection or credential
settings are accepted. Tokens and fetched values remain in memory and never
appear in output; only field names are exposed when requested.

The result contains `verified` and bounded `output`. Failure sets MCP
`isError: true`, including when diagnostics exceed 64 KiB. Verification does
not modify infrastructure, grants, roles, or policies.
