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

Add to `.claude/settings.json` (project or user level):

### Via Cloudflare tunnel (recommended)

```json
{
  "mcpServers": {
    "oberth": {
      "type": "url",
      "url": "https://watch.oberth.ci/mcp",
      "headers": {
        "Authorization": "Bearer oberth_xxxxxxxxxxxxxxxx"
      }
    }
  }
}
```

Cloudflare terminates TLS with a valid public certificate. No fingerprint
pinning is needed on the client side.

### Via direct NodePort

```json
{
  "mcpServers": {
    "oberth": {
      "type": "url",
      "url": "https://192.168.1.208:30443/mcp",
      "headers": {
        "Authorization": "Bearer oberth_xxxxxxxxxxxxxxxx"
      }
    }
  }
}
```

Direct access uses a self-signed certificate. The client must trust it.
Export the certificate and add it to the system trust store:

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
  --resolve "oberth:30443:192.168.1.208" \
  -H "Authorization: Bearer oberth_xxxxxxxxxxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  "https://oberth:30443/mcp"
```

A successful response returns 13 tools. For the tunnel path, use
`https://watch.oberth.ci/mcp` without `--cacert` or `--resolve`.

## MCP tools

| Tool | Description |
|------|-------------|
| `status` | CI status for a SHA or branch, including the failed step |
| `logs` | One named step's log output for a SHA |
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
