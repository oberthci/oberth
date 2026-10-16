# Seeding identities into OpenBao

Oberth keeps its private identities in OpenBao, never in Kubernetes Secrets.
A deployment from before that change mounts them from Secret volumes. Before
upgrading such a deployment, `oberth install --install-secretstore` checks that
every identity is already in OpenBao and refuses to replace the Deployment if
one is missing. Nothing is migrated automatically: seed them yourself, as an
administrator holding the OpenBao root (or an equivalently scoped) token.

| Legacy Secret | OpenBao KV path (mount `oberth`) | Fields |
|---|---|---|
| `oberth-tls` + `oberth-ssh-host-key` | `identities/<namespace>/server` | `tls.crt`, `tls.key`, `ssh_host_key` |
| `oberth-upstream-key` | `identities/<namespace>/oberth-upstream-key` | `id_ed25519`, `id_ed25519.pub` |
| `oberth-known-hosts` | `identities/<namespace>/oberth-known-hosts` | `known_hosts` |

The server bundle keeps the SSH host key, so agents' `known_hosts` entries stay
valid. With a production secret store the HTTPS certificate is then issued by
the server PKI and `tls.crt`/`tls.key` become a fallback only.

## Procedure

Send values as JSON on stdin: never put a token or key in a command line, where
the process table and the Kubernetes API audit log would record it. The token
travels the same way, as the first stdin line.

```bash
ns=oberth
secret() {  # secret <name>: the Secret's data, base64-decoded, as JSON
  kubectl -n "$ns" get secret "$1" -o json |
    python3 -c 'import base64,json,sys; d=json.load(sys.stdin)["data"]; print(json.dumps({k: base64.b64decode(v).decode() for k, v in d.items()}))'
}
put() {     # put <kv path>  (JSON object on stdin)
  { cat ~/.bao-root-token; echo; cat; } |
    kubectl exec -i -n openbao openbao-0 -c openbao -- \
      sh -c 'read -r BAO_TOKEN && export BAO_TOKEN && exec bao kv put -mount=oberth "$1" -' bao "$1"
}

# Server: TLS pair plus SSH host key, merged into one bundle.
python3 - "$(secret oberth-tls)" "$(secret oberth-ssh-host-key)" <<'EOF' | put "identities/$ns/server"
import json, sys
tls, ssh = json.loads(sys.argv[1]), json.loads(sys.argv[2])
print(json.dumps({"tls.crt": tls["tls.crt"], "tls.key": tls["tls.key"], "ssh_host_key": next(iter(ssh.values()))}))
EOF
secret oberth-upstream-key | put "identities/$ns/oberth-upstream-key"
secret oberth-known-hosts  | put "identities/$ns/oberth-known-hosts"
```

Check the field names (`bao kv get -mount=oberth -format=json identities/$ns/server
| jq '.data.data | keys'`), then rerun `oberth install --install-secretstore`. The
preflight compares the OpenBao certificate's public key with the one the
running pod serves and prints the SSH host key fingerprint for you to compare
with your `known_hosts`. Delete the legacy Secrets only after the upgraded
server has started from OpenBao.

## Restoring a database

A database restored into a fresh installation also needs the original
deployment's `compatibility` chart values (`schemaIdentity`, `auditDomain`,
`witnessKeyInfo`). Without them the server refuses the database
(`database schema is incompatible with this binary`). Pass them in a values
file on every install; `--reuse-values` keeps them afterwards.
