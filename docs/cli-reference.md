# Oberth command reference

Help rendered from every public command and subcommand. Both `-flag` and
`--flag` spellings are accepted; `--help` and `-h` return success. Remote
commands need the server settings described in [remote CLI](remote-cli.md).

## oberth access

```text
Usage: oberth access <list|allow|revoke> [flags]
Use oberth access <command> --help for command flags.
```

## oberth access allow

```text
Usage of access allow:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -namespace string
        Kubernetes namespace (default "oberth")
```

## oberth access list

```text
Usage of access list:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -json
        emit grants as a JSON array (machine-readable)
  -repo string
        filter by repository name; use upstream/org/name when ambiguous
  -revoked
        include revoked grants
```

## oberth access revoke

```text
Usage of access revoke:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -namespace string
        Kubernetes namespace (default "oberth")
```

## oberth admin

```text
Usage: oberth admin read-file <public-identity-path> [fallback-path]
Read allowed public identity files inside the Oberth pod.
```

## oberth admin read-file

```text
Usage: oberth admin read-file <public-identity-path> [fallback-path]
Read allowed public identity files inside the Oberth pod.
```

## oberth admission

```text
Usage: oberth admission [--json] | oberth admission release <run-id> [--force] [--json]
       oberth admission groups [--json]
       oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth admission groups

```text
Usage: oberth admission [--json] | oberth admission release <run-id> [--force] [--json]
       oberth admission groups [--json]
       oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth admission hold

```text
Usage: oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
Default allow: tag,release. Default duration: 2h; maximum: 12h. --allow '' holds all new admission.
```

## oberth admission release

```text
Usage: oberth admission [--json] | oberth admission release <run-id> [--force] [--json]
       oberth admission groups [--json]
       oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth artifacts

```text
Usage of artifacts:
  -data-root string
        Oberth data root holding the artifact store (default "/data")
  -o string
        shorthand for --output
  -output string
        save the artifact to this file (written atomically, digest-verified)
```

## oberth audit

```text
Usage: oberth audit [--tail] [--action <prefix>] [--actor <identity>] [--repo <repo>] [--since <time|24h>]
         [--until <time|24h>] [--limit N] [--before ID] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth audit head [--database <path>]   (in-pod)
       oberth audit record <action> [--details JSON]   (in-pod)
```

## oberth audit head

```text
Usage of audit head:
  -database string
        SQLite database path (default "/data/oberth.sqlite")
```

## oberth audit record

```text
Usage of audit record:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -details string
        JSON details to record (default "{}")
```

## oberth backup

```text
Usage: oberth backup [flags]

Back up this Oberth into one file in the current directory,
oberth-backup-<date-time>.tar.gz.enc: what a fresh install cannot
recreate. That is the history (runs, issues, promotions, uplinks) and the
secrets pipelines use (the OpenBao secrets you put in, policies, roles and the
grants). Server files (git caches, step logs), Helm values and what every
install makes for itself (its identities, CA) are not part of it. Nothing in
the cluster changes.

    oberth backup                    # asks for the OpenBao root token; Enter alone = history only
    oberth backup --history-only     # no secrets, no token
    oberth restore                   # later, on a fresh install: restores the newest backup here

The file is encrypted to ~/.config/oberth/backup/recipient.key, created by
the first backup. Keep a copy of that key OFF this machine: no backup can be
restored without it.

Also:
    oberth backup key [--print]           where the backup key is; --print shows it to store in a password manager
    oberth backup export-secrets [FILE]   the secrets as plain JSON any OpenBao/Vault can import
    oberth backup keygen [--dir DIR]      create the key pair ahead of time

Flags:
  --dry-run                    show what would be backed up; reads no secrets and writes nothing
  --history-only               back up runs, issues, promotions and uplinks only: no secrets, no OpenBao root token needed
  -o FILE                      shorthand for --output FILE
  --output FILE                write the backup to FILE instead of ./oberth-backup-<date-time>.tar.gz.enc
Advanced:
  --argo-namespace NAMESPACE   pipeline NAMESPACE (default oberth-argo)
  --context NAME               kubeconfig context NAME
  --kubeconfig FILE            kubeconfig FILE
  --max-database-bytes int     logical database byte limit (maximum 4 GiB; excludes WAL)
  --namespace NAMESPACE        Oberth NAMESPACE (default oberth)
  --openbao-namespace NAMESPACE OpenBao NAMESPACE (default openbao)
  --recipient CERT             encrypt to this PEM CERT instead of ~/.config/oberth/backup/recipient.pem
  --utility-image string       file-tool image for database helpers (default: installed utility image)

Guide: https://github.com/oberthci/oberth/blob/main/docs/backup-restore.md
```

## oberth backup database

```text
Usage of backup database:
  -database string
        SQLite database path (default "/data/oberth.sqlite")
  -max-database-bytes int
        logical snapshot byte limit (maximum 4 GiB) (default 1073741824)
```

## oberth backup export-secrets

```text
Usage: oberth backup export-secrets [FILE] [--output FILE.json]

Write the OpenBao KV secrets a backup holds as one plain JSON document
(format oberth.secrets/v1: KV v2 paths and their key/value data), which any
OpenBao or Vault can import with its own CLI. FILE defaults to the newest
backup in the current directory. The output is NOT encrypted: it refuses to
print to a terminal, and --output creates the file with mode 0600.

Import into a KV v2 mount with the bao (or vault) CLI:

    jq -c '.secrets[]' secrets.json | while read -r s; do
      echo "$s" | jq .data | bao kv put -mount=oberth "$(echo "$s" | jq -r .path)" -
    done

Flags:
  --output FILE                write to FILE (created 0600) instead of standard output
Advanced:
  --key KEY                    the backup's private KEY instead of ~/.config/oberth/backup/recipient.key
  --max-database-bytes int     logical database byte limit used for this backup (maximum 4 GiB)
```

## oberth backup key

```text
Usage: oberth backup key [--print]

Show the key every backup on this machine is encrypted to, which certificate
it belongs to (what oberth restore --inspect names), and whether you
confirmed a copy exists off this machine. --print shows the private key so
you can paste it into a password manager; it prints only to a terminal.
Restoring needs that file back as ~/.config/oberth/backup/recipient.key, or
next to the backup, or passed with --key.

Advanced:
  --print                      print the private key (PEM) to store it in a password manager; refuses anything but a terminal
```

## oberth backup keygen

```text
Usage: oberth backup keygen [--dir DIR]

Create the RSA-4096 key pair backups are encrypted to: recipient.key (the
private key, mode 0600) and recipient.pem (its certificate). oberth backup
does this itself on first use; existing files are never replaced.

Advanced:
  --dir DIR                    DIR for recipient.key and recipient.pem (created 0700)
```

## oberth branch

```text
Usage: oberth branch <prune|deletions> [flags]
Use oberth branch <command> --help for command flags.
```

## oberth branch deletions

```text
Usage: oberth branch prune <repository> [--merged-into <default-branch>] [--older-than <30d|72h>] [--exclude-prefix <prefix>]...
         [--branch <name>=<sha>]... [--allow-unmerged] [--report] [--confirm --plan-id <id>] [--confirm-reserved <upstream/org/repo:branch>]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth branch deletions [--repo <repository>] [--status pending|propagated|failed|skipped] [--batch <id>]
         [--limit N] [--before N] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth branch resolve <repository> --reason <text> [--id <record-id>]...
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
prune is a dry run unless --confirm names the dry run's plan ID. Mirror refs are deleted at once;
upstream deletion follows asynchronously. Read per-branch upstream state with branch deletions.
Each planned branch is classed merged, patch-equivalent, unique or unchecked; --report also lists
the kept ones. resolve closes failed deletion records (no ref changes); without --id only those
whose branch is gone from the mirror.
```

## oberth branch prune

```text
Usage: oberth branch prune <repository> [--merged-into <default-branch>] [--older-than <30d|72h>] [--exclude-prefix <prefix>]...
         [--branch <name>=<sha>]... [--allow-unmerged] [--report] [--confirm --plan-id <id>] [--confirm-reserved <upstream/org/repo:branch>]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth branch deletions [--repo <repository>] [--status pending|propagated|failed|skipped] [--batch <id>]
         [--limit N] [--before N] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth branch resolve <repository> --reason <text> [--id <record-id>]...
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
prune is a dry run unless --confirm names the dry run's plan ID. Mirror refs are deleted at once;
upstream deletion follows asynchronously. Read per-branch upstream state with branch deletions.
Each planned branch is classed merged, patch-equivalent, unique or unchecked; --report also lists
the kept ones. resolve closes failed deletion records (no ref changes); without --id only those
whose branch is gone from the mirror.
```

## oberth branch resolve

```text
Usage: oberth branch prune <repository> [--merged-into <default-branch>] [--older-than <30d|72h>] [--exclude-prefix <prefix>]...
         [--branch <name>=<sha>]... [--allow-unmerged] [--report] [--confirm --plan-id <id>] [--confirm-reserved <upstream/org/repo:branch>]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth branch deletions [--repo <repository>] [--status pending|propagated|failed|skipped] [--batch <id>]
         [--limit N] [--before N] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth branch resolve <repository> --reason <text> [--id <record-id>]...
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
prune is a dry run unless --confirm names the dry run's plan ID. Mirror refs are deleted at once;
upstream deletion follows asynchronously. Read per-branch upstream state with branch deletions.
Each planned branch is classed merged, patch-equivalent, unique or unchecked; --report also lists
the kept ones. resolve closes failed deletion records (no ref changes); without --id only those
whose branch is gone from the mirror.
```

## oberth fragments

```text
Usage: oberth fragments <list|show> [flags]
Use oberth fragments <command> --help for command flags.
```

## oberth fragments list

```text
Usage of fragments list:
  -data-root string
        Oberth data root holding the git cache (default "/data")
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
```

## oberth fragments show

```text
Usage of fragments show:
  -data-root string
        Oberth data root holding the git cache (default "/data")
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
```

## oberth git-ssh

```text
Usage: oberth git-ssh [SSH transport arguments]
Git invokes this pinned-key SSH transport through GIT_SSH.
It uses OBERTH_GIT_SSH_CONFIG and accepts only the configured native Git service.
```

## oberth init

```text
Usage of init:
  -force
        overwrite existing .oberth/build.yaml
```

## oberth install

```text
Usage of install:
  -accept-witness-genesis string
        one-shot acknowledgment of the existing deployment's exact audit chain head (<auditID>:<sha256hex>, from 'oberth audit head') when enabling --install-rekor on a deployment that already has audit history
  -also-upstream value
        another organization to connect with the same upstream key, host/organization or ssh:// URL (repeatable), e.g. codeberg.org/acme
  -argo-chart-version string
        argo-workflows chart version
  -argo-controller-profile string
        install the verified nonroot-static-v1 controller profile (requires the managed pinned chart)
  -argo-namespace string
        namespace pipelines execute in; must differ from --namespace (default: oberth-argo)
  -argo-vault-address string
        OpenBao/Vault base URL injected into credentialed pipeline containers as VAULT_ADDR (https only)
  -argo-vault-credentialed-role string
        Kubernetes-auth role credentialed pipelines log in with; bind it to exactly (--argo-namespace, the credentialed ServiceAccount)
  -chart string
        install the Oberth chart from this local directory or archive instead of the published repository
  -chart-version string
        Oberth chart version (default: binary version)
  -client-access string
        write this machine's client files without asking: both, cli, mcp, or none
  -client-host string
        address clients use to reach this server, a DNS name or IP (default: the Kubernetes API host, or localhost); client files, SSH config and printed URLs use it and the certificate covers it
  -context string
        context in the selected kubeconfig
  -create-kind
        create or use the local oberth kind cluster
  -credentialed-secret-path oberth access allow
        full KV data path approved in the secret-access table (repeatable, exactly the string oberth access allow records, e.g. oberth/data/release/cosign-secret); synced into the RELEASE-tier credentialed Vault policy as an exact read grant when --install-secretstore reconciles the store — the branch-tier ci-secrets policy never receives grants
  -dry-run
        print plan without making changes
  -f value
        shorthand for --values
  -forge string
        upstream name (github, codeberg, or gitlab)
  -image string
        server image the chart deploys, overriding the chart default (used with --chart for local iteration)
  -install-rekor
        also install a local Rekor transparency-log stack (MySQL + Trillian + Rekor) for the audit witness
  -install-secretstore
        also install a production-mode OpenBao (persistent storage; verified TLS bootstrapped directly on its PVC; initialized and unsealed via kubectl exec, root token and unseal key printed once) and configure it as Oberth's release secret store
  -install-secretstore-dev
        also install a dev-mode OpenBao (in-memory, auto-unsealed, well-known root token) and configure it as Oberth's release secret store — evaluation only
  -kubeconfig string
        kubeconfig file for an existing cluster
  -namespace string
        Oberth namespace (default: oberth)
  -network-policy string
        pipeline egress NetworkPolicy: auto (default: enable, including k3s; preserve existing values), true (always enable), false (always disable)
  -openbao-chart-version string
        OpenBao chart version
  -openbao-namespace string
        OpenBao namespace (default: openbao)
  -organization string
        upstream organization
  -push-banner-url string
        push feedback URL; omitted preserves the live URL, explicit empty clears it
  -rekor-chart-version string
        Rekor chart version
  -rekor-namespace string
        Rekor namespace (default: rekor)
  -service-type string
        Oberth Service type: NodePort (default for a fresh install; clients use NodePorts 30022/30443), LoadBalancer (on k3s, servicelb also takes host ports 2222 and 443 on the node) or ClusterIP; an upgrade keeps the release's current type unless this is set
  -skip-argo-workflows
        do not install the Argo Workflows controller, because this cluster already runs one watching --argo-namespace (the install still binds Oberth to it and still verifies it is ready)
  -ssh-public-key string
        workstation SSH public key path
  -timeout duration
        wait timeout for pod readiness (default 15m0s)
  -tls-extra-dns-name value
        additional DNS name the generated server certificate is issued for (repeatable), beyond the in-cluster names; a client reaching the server on any other name hits a hostname mismatch that no trust anchor can fix. A macOS kind install adds localhost automatically
  -tls-extra-ip value
        additional IP address the generated server certificate is issued for (repeatable); an address reached as an IP needs an IP SAN, because a DNS name never matches one. A macOS kind install adds 127.0.0.1 automatically
  -tls-remove-dns-name value
        remove a permitted DNS name; refused while the served certificate contains it (repeatable)
  -tls-remove-ip value
        remove a permitted IP; refused while the served certificate contains it (repeatable)
  -upgrade
        force helm upgrade of existing releases even when already up to date (re-runs auto-upgrade when a newer chart is targeted; this flag forces reconciliation)
  -uplink-identity string
        workstation identity
  -upstream-key-file string
        existing work SSH private key to import into OpenBao
  -upstream-url string
        organization upstream base URL, e.g. ssh://git@github.com/oberthci
  -utility-image string
        file-tool image for cache preparation and database helpers (air-gapped mirror)
  -values value
        helm values file applied to the Oberth release (repeatable, applied in order); the installer's own settings still win, so a value it computed cannot be overridden by a stale one in a file
  -yes
        proceed even for non-local clusters
```

## oberth issues

```text
Usage: oberth issues [flags]
  -active-stale-after string
        override the active stale window (duration)
  -json
        emit the server payload
  -stale-after string
        override the general stale window (duration)
  -state string
        filter open, closed or all issues (default "open")
  -work-status string
        filter todo, active, waiting or review
```

## oberth issues update

```text
Usage: oberth issues update --id <issue-id> --work-status <status> [flags]
  -expected-revision int
        optional current metadata revision
  -id int
        issue ID
  -work-status string
        todo, active, waiting or review
```

## oberth log

```text
Usage: oberth log <run-id> [--burn <burn> --step <step>] [flags]
With no pattern, limit, tail, offset or byte-offset: last 200 lines. Pass --tail --limit N for a larger tail, or --limit 0 for the existing bounded full read.
  -burn string
        burn name
  -byte-offset int
        absolute log cursor from next_byte_offset
  -context int
        lines of context around each match
  -json
        emit JSON, including the default window when used
  -limit int
        return at most this many
  -offset int
        skip this many
  -pattern string
        RE2 pattern; offset and limit then page over matches
  -raw
        keep the [burn/step] prefix on each line and the Argo executor lines
  -step string
        step name
  -tail
        read from the end
```

## oberth mcp

```text
Usage: oberth mcp <command>

Commands:
  tools     List available MCP tools
  call      Call an MCP tool

Repository arguments accept upstream/org/name (for example codeberg/acme/widget).
Bare names and org/name work only when unique; ambiguity errors list candidates.
Run and promotion IDs accept unique hexadecimal prefixes of at least 8 characters.
Issue IDs are full decimal integers.

Promotion:
  oberth mcp call promote '{"repo":"org/repo","sha":"<full-sha>","branch":"main"}'
  Use sources:[{"sha":"<full-sha>"},...] instead of sha for a merge train.
  Authorized park refs (default refs/heads/park/) may skip branch CI; supply repo.
  Parked sources need an admin, an exact grant, or upstream promotion policy opt-in.
  Single-source and train promotions always test parked sources in merged-tree CI.
  Red branch evidence and target require_branch_ci policy still refuse promotion.
  Use promote_status with the returned ID to follow the result.

Environment:
  OBERTH_MCP_URL        MCP server URL (overrides OBERTH_BASE_URL)
  OBERTH_BASE_URL       Server URL; /mcp is appended when no MCP URL is set
  OBERTH_TOKEN          Bearer token for authentication
  OBERTH_TOKEN_COMMAND  Command that prints a token when OBERTH_TOKEN is unset
  OBERTH_CA_CERT        Inline PEM or CA certificate file path (overridden by --ca-file)

Flags:
  --url <url>          MCP server URL
  --token-file <path>  Path to a file containing the bearer token (0600)
  --ca-file <path>     PEM CA certificate for TLS verification
  --timeout <duration> Client deadline override (e.g. 90s or 11m); by default
                       at least 60s, or the tool timeout plus 30s for long polls
  --reconnect-window <duration>  Read reconnect window across server restarts
  -o <path>           Alias for --output
  --args-file <path>   Read tool arguments JSON from a file (or "-" for stdin);
                       bypasses the shell argv length ceiling for large payloads
  --output <path>      artifact_get only: stream the artifact's exact bytes to
                       this file instead of an inline text view; the size and
                       SHA-256 are verified and nothing is left on failure
```

## oberth mcp call

```text
Usage: oberth mcp call [arguments] [flags]
  -args-file string
        JSON file or - for stdin
  -ca-file string
        CA file
  -o string
        output file
  -output string
        output file
  -reconnect-window string
        positive reconnect duration
  -timeout string
        positive request duration
  -token-file string
        token file
  -url string
        MCP URL
```

## oberth mcp tools

```text
Usage: oberth mcp tools [arguments] [flags]
  -args-file string
        JSON file or - for stdin
  -ca-file string
        CA file
  -o string
        output file
  -output string
        output file
  -reconnect-window string
        positive reconnect duration
  -timeout string
        positive request duration
  -token-file string
        token file
  -url string
        MCP URL
```

## oberth outbox

```text
Usage: oberth outbox list|show|retry|abandon [<upstream/org/repo>] [--id <record-id>] [--reason <text>]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
Repair a receive record that blocks its repository: show it, retry delivery, or abandon it
(moved aside under abandoned/, audited). Unscoped admin only.
```

## oberth preflight

```text
Usage: oberth preflight <drift|pki|upstreams> [flags]
Use oberth preflight <command> --help for command flags.
```

## oberth preflight drift

```text
Usage of preflight drift:
  -chart string
        path to the local chart directory (default "charts/oberth")
  -context string
        kubeconfig context (default: same resolution as the oberth binary)
  -namespace string
        helm release namespace (default "oberth")
  -release string
        helm release name (default "oberth")
```

## oberth preflight pki

```text
Usage of preflight pki:
  -context string
        kubeconfig context
  -namespace string
        Oberth namespace (default "oberth")
  -openbao-namespace string
        OpenBao namespace (default "openbao")
  -tls-extra-dns-name value
        expected DNS name (repeatable)
  -tls-extra-ip value
        expected IP SAN (repeatable)
```

## oberth preflight upstreams

```text
Usage of preflight upstreams:
  -inventory string
        complete old-server upstream list metadata file
```

## oberth promotion

```text
Usage: oberth promotion create --branch <target> [--sha <sha> | --sources-file <json-file>] [--repo <repository>] [--skip-conflicting] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
Choose exactly one of --sha or --sources-file.
sources-file contains 1..32 ordered {"sha":"...","ref":"feature/one"} objects in a JSON array.
Each source must be an exactly tested green branch commit. A train merges in order and runs CI once before publishing.
Example sources.json (replace with your three tested SHAs):
[{"sha":"1111111111111111111111111111111111111111"},{"sha":"2222222222222222222222222222222222222222"},{"sha":"3333333333333333333333333333333333333333"}]
oberth promotion create --repo codeberg/acme/widgets --branch main --sources-file sources.json
Usage: oberth promotion retry <promotion-id> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth promotion retry

```text
Usage: oberth promotion retry <promotion-id> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth promotion create

```text
Usage: oberth promotion create --branch <target> [--sha <sha> | --sources-file <json-file>] [--repo <repository>] [--skip-conflicting] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
Choose exactly one of --sha or --sources-file.
sources-file contains 1..32 ordered {"sha":"...","ref":"feature/one"} objects in a JSON array.
Each source must be an exactly tested green branch commit. A train merges in order and runs CI once before publishing.
Example sources.json (replace with your three tested SHAs):
[{"sha":"1111111111111111111111111111111111111111"},{"sha":"2222222222222222222222222222222222222222"},{"sha":"3333333333333333333333333333333333333333"}]
oberth promotion create --repo codeberg/acme/widgets --branch main --sources-file sources.json
```

## oberth publication

```text
Usage: oberth publication abandon <publication-id> --reason <text> [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth publication abandon

```text
Usage: oberth publication abandon <publication-id> --reason <text> [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth repo

```text
Usage: oberth repo <add|remove|archive|unarchive|verify> [flags]
Use oberth repo <command> --help for command flags.
```

## oberth repo add

```text
Usage of repo add:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -default-branch string
        repository default branch until the first push confirms it (default "main")
```

## oberth repo archive

```text
Usage: oberth repo archive <repository> [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth repo remove

```text
Usage of repo remove:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -git-cache-root string
        root directory for bare Git caches (default "/data/git")
```

## oberth repo unarchive

```text
Usage: oberth repo unarchive <repository> [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth repo verify

```text
Usage of repo verify:
  -all
        verify every registered repository
  -database string
        SQLite database path (default "/data/oberth.sqlite")
  -git-cache-root string
        root directory for bare Git caches (default "/data/git")
  -known-hosts string
        pinned upstream known_hosts (default "/run/oberth-identities/hosts/known_hosts")
  -upstream-key string
        upstream SSH private key (default "/run/oberth-identities/upstream/id_ed25519")
```

## oberth repos

```text
Usage: oberth repos [flags]
  -json
        emit the server's payload unchanged
```

## oberth resource

```text
Usage: oberth resource <grant|get|revoke> [flags]
Use oberth resource <command> --help for command flags.
```

## oberth resource get

```text
Usage of resource get:
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
```

## oberth resource grant

```text
Usage of resource grant:
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
  -limits-cpu string
        administrator ceiling as a Kubernetes quantity
  -limits-ephemeral-storage string
        administrator ceiling as a Kubernetes quantity (default "16Gi")
  -limits-memory string
        administrator ceiling as a Kubernetes quantity
  -requests-cpu string
        administrator ceiling as a Kubernetes quantity
  -requests-ephemeral-storage string
        administrator ceiling as a Kubernetes quantity (default "16Gi")
  -requests-memory string
        administrator ceiling as a Kubernetes quantity
```

## oberth resource revoke

```text
Usage of resource revoke:
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
```

## oberth restore

```text
Usage: oberth restore [FILE | DIR] [flags]

Restore an oberth backup onto this Oberth, typically a fresh install. Without
an argument it takes the newest oberth-backup-*.tar.gz.enc in the current
directory; a directory means the newest backup in it. It brings back the
history and the secrets pipelines use, and nothing else: this install keeps
its own identities, CA and Helm values. It first shows what the backup holds
and exactly what would change here (history counts before and after, secrets
new or replaced by name), then asks you to type the cluster's context name.
This install's database is kept on its volume in /data/pre-restore-<time>/.

    oberth restore --inspect         # what the newest backup here holds; no cluster needed
    oberth restore --dry-run         # what would change on this install; changes nothing
    oberth restore                   # restore it (asks for the OpenBao root token and a confirmation)
    oberth restore --only history    # runs, issues, uplinks only; keeps this install's secrets

Flags:
  --dry-run                    show what would change on this install, then stop
  --inspect                    show what the backup holds and stop; needs no cluster
  --only PARTS                 restore only these PARTS: history, secrets (comma-separated)
  --yes                        skip the confirmation (for scripts)
Advanced:
  --argo-namespace NAMESPACE   pipeline NAMESPACE (default oberth-argo)
  --chart CHART                local Oberth CHART for the Helm upgrade (default: the published chart at the running version)
  --context NAME               kubeconfig context NAME
  -i FILE                      shorthand for --input FILE
  --input FILE                 the backup FILE (same as the argument)
  --key KEY                    the backup's private KEY instead of ~/.config/oberth/backup/recipient.key
  --kubeconfig FILE            kubeconfig FILE
  --max-database-bytes int     logical database byte limit used for this backup (maximum 4 GiB)
  --namespace NAMESPACE        Oberth NAMESPACE (default oberth)
  --openbao-namespace NAMESPACE OpenBao NAMESPACE (default openbao)
  --private-key KEY            same as --key KEY
  --timeout duration           wait timeout for pods
  --utility-image string       file-tool image for database helpers (default: installed utility image)

Guide: https://github.com/oberthci/oberth/blob/main/docs/backup-restore.md
```

## oberth run

```text
Usage: oberth run <run-id> [flags]
  -json
        emit the server's payload unchanged
```

## oberth run cancel

```text
Usage: oberth run cancel <run-id> [--reason <text>] [--force] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>] | [--database <path>]
```

## oberth run purge

```text
Usage: oberth run purge <run-id> --what logs|artifacts|both --reason <text> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth run rerun

```text
Usage: oberth run rerun <run-id> [--reason <text>] [--force] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth run resources

```text
Usage of run resources:
  -burn string
        optional burn
  -format string
        json or yaml (default "json")
  -include-failed
        include failed and incomplete runs
  -json
        JSON report (default)
  -last int
        last N runs, maximum 100 (default 30)
  -ref string
        exact ref; default includes default branch and its promotion runs; any includes all refs
  -repo string
        registered repository (optional with --run); use upstream/org/name when ambiguous
  -run string
        exact run ID or unique prefix
  -step string
        optional template name
  -yaml
        YAML report with paste-ready resources
```

## oberth run retry

```text
Usage: oberth run retry <run-id> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
```

## oberth runs

```text
Usage: oberth runs [flags]
  -active
        only queued and running runs, in scheduling order, with queue position and wait reason
  -all
        follow pages to the oldest run
  -before string
        only runs older than this cursor (printed after a full page)
  -json
        emit the server's payload unchanged
  -limit int
        runs per page (at most 100) (default 20)
  -ref string
        only this branch or tag
  -repo string
        only this repository; use upstream/org/name when ambiguous
```

## oberth runs cancel

```text
Usage: oberth runs cancel --repo <repo> --ref <branch> [--reason <text>] [--force] [--json]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>] | [--database <path>]
```

## oberth schedules

```text
Usage of schedules:
  -data-root string
        Oberth data root holding the git cache (default "/data")
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
  -max-entries int
        most entries one repository may declare (default 8)
  -min-interval duration
        shortest interval a repository may schedule itself at (default 15m0s)
```

## oberth schedules list

```text
Usage: oberth schedules list [repo] [--json]
       oberth schedules pause <repo> <entry> [--reason <text>]
       oberth schedules resume <repo> <entry>
       oberth schedules trigger <repo> <entry> [--force]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth schedules [repository]   (in-pod listing of the schedule file)
```

## oberth schedules pause

```text
Usage: oberth schedules list [repo] [--json]
       oberth schedules pause <repo> <entry> [--reason <text>]
       oberth schedules resume <repo> <entry>
       oberth schedules trigger <repo> <entry> [--force]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth schedules [repository]   (in-pod listing of the schedule file)
```

## oberth schedules resume

```text
Usage: oberth schedules list [repo] [--json]
       oberth schedules pause <repo> <entry> [--reason <text>]
       oberth schedules resume <repo> <entry>
       oberth schedules trigger <repo> <entry> [--force]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth schedules [repository]   (in-pod listing of the schedule file)
```

## oberth schedules trigger

```text
Usage: oberth schedules list [repo] [--json]
       oberth schedules pause <repo> <entry> [--reason <text>]
       oberth schedules resume <repo> <entry>
       oberth schedules trigger <repo> <entry> [--force]
         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]
       oberth schedules [repository]   (in-pod listing of the schedule file)
```

## oberth secretstore

```text
Usage: oberth secretstore <setup|verify|sync|exec|bootstrap-tls> [flags]
Use oberth secretstore <command> --help for command flags.
```

## oberth secretstore bootstrap-tls

```text
Usage of secretstore bootstrap-tls:
  -namespace string
        OpenBao Kubernetes namespace
  -output-dir string
        absolute OpenBao PVC directory for TLS material
```

## oberth secretstore exec

```text
Usage: oberth secretstore exec --dir DIR --path PATH -- COMMAND [ARG...]
```

## oberth secretstore setup

```text
Usage of secretstore setup:
  -print-script
        write the embedded setup script to stdout
```

## oberth secretstore sync

```text
Usage of secretstore sync:
  -argo-namespace string
        pipeline namespace (default "oberth-argo")
  -context string
        kubeconfig context (default: current context)
  -namespace string
        Oberth namespace (default "oberth")
  -openbao-namespace string
        OpenBao namespace (default "openbao")
```

## oberth secretstore verify

```text
Usage of secretstore verify:
  -address string
        OpenBao API base URL (default: discovered from the running serve process)
  -ca-cert string
        optional PEM file pinning the OpenBao TLS trust anchors
  -expect value
        expected fields: <path-base>/<field>[,<field>,...] (repeatable, implies -keys)
  -insecure-http
        DEVELOPMENT ONLY: allow a plain-HTTP OpenBao address
  -k8s-auth-mount string
        OpenBao Kubernetes auth mount path
  -keys
        verify or list KV field names; use with --expect to assert
  -kv-mount string
        KV v2 mount backing the virtual oberth/upstream/ namespace (default: discovered from serve, else oberth)
  -path value
        KV path to verify (repeatable; alternative to positional paths)
  -release-tier
        verify the release-tier OpenBao trust chain (Argo pipeline namespace, release ServiceAccount, --argo-vault-* flags)
  -repo string
        verify a per-repo identity (upstream/org/repo); with --release-tier, overrides the shared SA
  -role string
        OpenBao Kubernetes auth role
  -sa-token string
        optional ServiceAccount token path override
  -tier string
        which per-repo tier to verify: release or ci (only with --repo or --release-tier) (default "release")
  -timeout duration
        overall verification deadline (default 45s)
```

## oberth serve

```text
Usage of serve:
  -accept-witness-chain-reset string
        one-shot acknowledgment: exact Rekor UUID of the latest published witness before a fresh genesis permanently abandons that chain
  -accept-witness-genesis string
        one-shot acknowledgment: exact current audit chain head as <auditID>:<sha256hex>; marks it as the witness genesis so external witnessing starts there, explicitly trusting all prior local history (enables Rekor on an existing deployment)
  -admission-cpu-millis int
        deprecated compatibility option; ignored by workflow admission (default 8000)
  -admission-disk-floor-bytes int
        when enabled and free bytes on /data are below this floor, admission is held until disk frees up (#673)
  -admission-extended-resources string
        deprecated compatibility option; ignored by workflow admission (default "{}")
  -admission-io-runs int
        deprecated compatibility option; ignored by workflow admission
  -admission-legacy-resources string
        deprecated compatibility option; ignored by workflow admission (default "{}")
  -admission-max-head-wait duration
        deprecated compatibility option; ignored by workflow admission (default 20m0s)
  -admission-memory-bytes int
        deprecated compatibility option; ignored by workflow admission (default 17179869184)
  -admission-memory-headroom-bytes int
        deprecated compatibility option; ignored by workflow admission (default 1073741824)
  -admission-memory-headroom-percent int
        deprecated compatibility option; ignored by workflow admission (default 10)
  -admission-pressure-cpu-some-avg10 float
        deprecated compatibility option; ignored by workflow admission (default 30)
  -admission-pressure-cpu-some-avg60-max float
        deprecated compatibility option; ignored by workflow admission
  -admission-pressure-enabled
        hold new workflow grants at the hard host CPU/IO PSI limits (default true)
  -admission-pressure-hard-cpu float
        hard host CPU some PSI avg10 percentage for new workflow grants (default 50)
  -admission-pressure-hard-io float
        hard host IO some PSI avg10 percentage for new workflow grants (default 40)
  -admission-pressure-hysteresis float
        deprecated compatibility option; ignored by workflow admission (default 10)
  -admission-pressure-io-full-avg60-max float
        deprecated compatibility option; ignored by workflow admission
  -admission-pressure-io-some-avg10 float
        deprecated compatibility option; ignored by workflow admission (default 20)
  -admission-pressure-max-defer duration
        deprecated compatibility option; ignored by workflow admission (default 20m0s)
  -admission-pressure-max-wait-seconds int
        deprecated compatibility option; ignored by workflow admission (default 1200)
  -admission-pressure-mode string
        deprecated compatibility option; ignored by workflow admission (default "enforce")
  -admission-size-weights string
        deprecated compatibility option; ignored; Kubernetes places step pods (default "S=1,M=2,L=3,XL=0")
  -admission-stall-timeout duration
        workflow startup bound for absent or non-capacity-stalled pods; Kubernetes capacity waits are exempt (default 10m0s)
  -argo-ci-secrets-serviceaccount string
        ServiceAccount for CI templates with approved upstream-scoped secrets; its Vault role never carries release grants (default "oberth-argo-ci-secrets")
  -argo-controller-profile string
        exact controller profile to verify before nonroot submission; empty disables nonroot
  -argo-credentialed-serviceaccount string
        ServiceAccount for release templates with approved secrets; carries a projected token for OpenBao Kubernetes auth (default "oberth-argo-credentialed")
  -argo-execution-placement-config string
        absolute path to administrator-owned isolated execution placement JSON
  -argo-execution-target-config string
        absolute path to administrator-owned branch execution target policy
  -argo-executor-scratch-size-limit string
        per-Pod Argo executor scratch size limit (Kubernetes quantity) (default "1Gi")
  -argo-executor-serviceaccount string
        ServiceAccount for Argo's own executor containers; must differ from all pipeline identities (default "oberth-argo-executor")
  -argo-goproxy-ca string
        PEM CA certificate that signed the Go module proxy's TLS certificate; delivered to pipeline pods
  -argo-goproxy-cert string
        TLS certificate for the Go module proxy
  -argo-goproxy-key string
        TLS private key for the Go module proxy
  -argo-goproxy-listen string
        listen address for the read-only Go module proxy (e.g. :8444); empty disables the proxy (#662)
  -argo-goproxy-module-prefix string
        Administrator-owned private module namespace
  -argo-goproxy-organization string
        Exact organization owning private module repositories
  -argo-goproxy-pseudo-non-default-branches
        Allow pseudo-versions from non-default public branches and existing allowed tags; default is default-branch-only
  -argo-goproxy-repository-prefix string
        Repository-name prefix within the configured owner
  -argo-goproxy-upstream string
        Exact upstream owning private module repositories
  -argo-goproxy-url string
        HTTPS URL pipeline pods use to reach the module proxy (e.g. https://oberth-goproxy.oberth.svc:8444)
  -argo-namespace string
        namespace for Argo Workflow pipelines (required)
  -argo-pipeline-serviceaccount string
        ServiceAccount for pipeline templates without approved secrets; no Vault role, no token (default "oberth-argo-pipeline")
  -argo-release-tag-routes-config string
        absolute path to administrator-owned release tag pattern -> workflow file JSON
  -argo-release-wif-config string
        absolute path to administrator-owned public JSON release WIF capabilities (empty disables federation)
  -argo-repo-workflow-timeout value
        administrator repository deadline cap: upstream/org/repo=30m (repeatable)
  -argo-resource-sampler-config string
        administrator-owned kubelet sampler JSON config; disabled when empty
  -argo-source-storage-class string
        storage class for per-run pipeline source volumes (empty: cluster default)
  -argo-vault-address string
        OpenBao API base URL injected into credentialed Argo containers as VAULT_ADDR (HTTPS)
  -argo-vault-ca-cert string
        PEM file pinning the trust anchors credentialed pipeline containers verify --argo-vault-address against
  -argo-vault-ci-secrets-role string
        OpenBao Kubernetes auth role for CI pipelines with approved upstream-scoped secrets; bound to the CI-secrets ServiceAccount and namespace (empty refuses CI credentialed pipelines at admission)
  -argo-vault-credentialed-role string
        OpenBao Kubernetes auth role for credentialed release pipelines; bound to the credentialed ServiceAccount and namespace
  -argo-workflow-timeout duration
        ceiling on a Workflow's own activeDeadlineSeconds (default 12h0m0s)
  -argo-workflow-ttl int
        finished Workflow retention in seconds (default 3600)
  -artifact-retention duration
        maximum age of terminal run artifacts (0 disables age retention)
  -artifacts-budget-bytes int
        total artifact storage before the oldest runs are evicted (default 4294967296)
  -artifacts-limit-bytes int
        maximum total bytes of artifacts kept per run (default 268435456)
  -audit-anchor-interval duration
        external audit checkpoint interval (default 10m0s)
  -audit-anchor-max-age duration
        maximum checkpoint age allowed by readiness (default 30m0s)
  -audit-gate-timeout duration
        total audit mutation gate budget, including queueing and retries (default 2m0s)
  -audit-gate-verify-timeout duration
        deadline for each fresh audit mutation verification attempt (default 45s)
  -audit-rekor-ca string
        optional PEM file pinning the Rekor HTTPS TLS trust anchors
  -audit-rekor-insecure-http
        DEVELOPMENT ONLY: allow a plain-HTTP Rekor URL
  -audit-rekor-pubkey string
        optional PEM public key pinning the Rekor log identity instead of Sigstore TUF
  -audit-rekor-url string
        Rekor transparency log HTTPS URL (empty disables the external witness)
  -audit-tsa-ca string
        optional PEM file pinning the TSA HTTPS TLS trust anchors
  -audit-tsa-insecure-http
        DEVELOPMENT ONLY: allow a plain-HTTP TSA URL
  -audit-tsa-roots string
        optional PEM file containing pinned TSA trust roots
  -audit-tsa-url string
        RFC 3161 timestamp authority HTTPS URL (empty disables external timestamps)
  -auto-retry-infrastructure
        automatically retry eligible infrastructure failures once at their original queue position (default true)
  -ci-cache-root string
        CI host cache root (default "/var/cache/oberth/ci")
  -data string
        persistent data root (default "/data")
  -database string
        SQLite database path (default "/data/oberth.sqlite")
  -disable-declared-admission
        deprecated compatibility option; ignored by workflow admission
  -fragment-allowlist string
        comma-separated repositories usable as pipeline fragments; empty permits every registered repository
  -git-gc-interval duration
        periodic Git GC interval (default 24h0m0s)
  -git-timeout duration
        Git command timeout (default 10m0s)
  -https-listen string
        HTTPS listen address (default ":8443")
  -issue-active-stale-after duration
        active issue stale window since last update (no longer than issue-stale-after) (default 1h0m0s)
  -issue-stale-after duration
        open issue stale window since last update (default 168h0m0s)
  -known-hosts string
        upstream known_hosts (default "/etc/oberth/known-hosts/known_hosts")
  -kubernetes-client-burst int
        Kubernetes API burst allowance per server client (default 100)
  -kubernetes-client-qps float
        Kubernetes API requests per second per server client (default 50)
  -log-retention duration
        maximum age of terminal run logs (0 disables age retention)
  -logs-budget-bytes int
        total retained log budget (0 disables size retention)
  -max-concurrent-jobs int
        maximum concurrent workflows (boot default) (default 3)
  -max-concurrent-jobs-file string
        mounted ConfigMap key to reload max concurrent Jobs without restart; invalid or missing keeps the last good value
  -namespace string
        Kubernetes namespace (default "oberth")
  -pipeline-network-policy-enabled value
        configured chart networkPolicy.enabled (true or false); omitted means unavailable, not measured CNI enforcement
  -promotion-grant-max-lifetime duration
        maximum lifetime for promotion grants (default 24h, hard ceiling 168h) (default 24h0m0s)
  -push-banner-url string
        optional HTTPS dashboard URL echoed to every accepted git push
  -release-cache-root string
        release host cache root (default "/var/cache/oberth/release")
  -reserved-branches string
        comma-separated branch globs that, like the default branch, no push or prune may delete (default "main,master")
  -retention-keep-newest int
        terminal runs retained per repository/ref during automatic sweeps (default 1)
  -retention-sweep-interval duration
        interval between protected evidence retention sweeps (default 10m0s)
  -run-log-limit-bytes int
        maximum retained log bytes per run (default 67108864)
  -runner-image-prefixes string
        comma-separated allowlist of permitted runner image prefixes (default "golang:,debian:,aquasec/trivy:")
  -schedule-max-entries int
        most schedule entries one repository may declare (default 8)
  -schedule-min-interval duration
        shortest interval a repository may schedule itself at (default 15m0s)
  -secretstore-address string
        OpenBao API base URL for store-sourced release secrets (HTTPS)
  -secretstore-ca-cert string
        optional PEM file pinning the OpenBao TLS trust anchors
  -secretstore-insecure-http
        DEVELOPMENT ONLY: allow a plain-HTTP OpenBao address
  -secretstore-k8s-auth-mount string
        OpenBao Kubernetes auth mount path (default "kubernetes")
  -secretstore-kv-mount string
        KV v2 mount backing the virtual oberth/upstream/ secret namespace (default "oberth")
  -secretstore-path value
        allowlisted OpenBao KV path (repeatable)
  -secretstore-role string
        OpenBao Kubernetes auth role bound to the Oberth ServiceAccount
  -secretstore-sa-token string
        optional ServiceAccount token path override for the OpenBao login
  -secretstore-transit-key string
        OpenBao transit key for trusted Plan artifacts
  -secretstore-transit-mount string
        OpenBao transit mount for trusted Plan artifacts (enables trusted Plan/Apply with --secretstore-transit-key)
  -source-allowlist string
        JSON map of consumer upstream/org/repository to allowed pinned source repositories; empty denies all (default "{}")
  -ssh-host-key string
        SSH host private key (default "/etc/oberth/ssh/ssh_host_key")
  -ssh-listen string
        SSH listen address (default ":2222")
  -tls-cert string
        TLS certificate (default "/etc/oberth/tls/tls.crt")
  -tls-key string
        TLS private key (default "/etc/oberth/tls/tls.key")
  -uplink-token-default-ttl duration
        default lifetime for new uplink bearer tokens (90 days) (default 2160h0m0s)
  -uplink-token-max-ttl duration
        maximum new uplink bearer lifetime (hard ceiling 8760h / 365 days) (default 8760h0m0s)
  -upstream-key string
        upstream SSH private key (default "/etc/oberth/upstream-key/id_ed25519")
```

## oberth setup

```text
Usage of setup:
  -accessible
        screen reader mode (sequential prompts, no cursor addressing)
  -chart string
        local chart for development
  -dry-mode
        run the wizard fully, print the equivalent oberth install command, exit without applying
  -fresh
        ignore the answers saved by a previous run (~/.config/oberth/setup.json) and start over
  -image string
        local server image for development
  -plain
        no TUI, sequential prompts (auto-selected for non-TTY, TERM=dumb, NO_COLOR)
```

## oberth size

```text
Usage: oberth size <grant|get|revoke> [flags]
Use oberth size <command> --help for command flags.
```

## oberth size get

```text
Usage of size get:
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
```

## oberth size grant

```text
Usage of size grant:
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
  -max string
        largest admission size the repository may declare: L or XL
```

## oberth size revoke

```text
Usage of size revoke:
  -database string
        SQLite database path (in-pod) (default "/data/oberth.sqlite")
```

## oberth status

```text
Usage: oberth status [flags]
  -json
        emit the server's payload unchanged
```

## oberth upgrade

```text
Usage of upgrade:
  -allow-retired-execution
        override retired resource/repository checks; accept orphaned resources and refused legacy runs
  -argo-controller-profile string
        explicitly reconcile the managed nonroot-static-v1 Argo profile and verify it before enabling server admission
  -chart string
        override the chart reference (for testing)
  -context string
        kubeconfig context to target (default: current-context)
  -dry-run
        show what would change without doing it
  -f value
        alias for --values
  -namespace string
        Oberth namespace (default: oberth)
  -openbao-namespace string
        OpenBao namespace (default: openbao)
  -ownership-handoff string
        verified connector/site ownership handoff receipt required before retirement
  -set value
        one canonical dotted key=scalar per flag; escape literal commas (repeatable)
  -timeout duration
        wait timeout for rollout completion (default 5m0s)
  -tls-extra-dns-name value
        add a server TLS DNS name (repeatable)
  -tls-extra-ip value
        add a server TLS IP SAN (repeatable)
  -tls-remove-dns-name value
        remove a permitted DNS name; refused while served (repeatable)
  -tls-remove-ip value
        remove a permitted IP SAN; refused while served (repeatable)
  -values value
        values file to pass to helm upgrade (repeatable)
  -wait-for string
        enable waiting for all or critical (tag/promotion/plan/apply/unknown) runs
  -wait-idle
        wait for all queued/running runs before upgrading (advisory, not an admission freeze)
  -wait-timeout duration
        maximum queue wait, independent of --timeout (default 2h0m0s)
  -yes
        proceed without confirmation for non-local targets
```

## oberth uplink

```text
Usage: oberth uplink <add|list|remove|rotate|scope> [flags]
Use oberth uplink <command> --help for command flags.
```

## oberth uplink add

```text
Usage of uplink add:
  -admin
        Grant admin privileges (required for access_allow/access_revoke)
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -scope value
        limit the uplink to these upstreams' repositories (repeatable or comma-separated; default: every repository)
  -tls-cert string
        HTTPS certificate path (in-pod; requires the live admin daemon) (default "/run/oberth-identities/server/tls.crt")
  -ttl string
        bearer token lifetime (positive Go duration; default and maximum come from the live server)
```

## oberth uplink list

```text
Usage of uplink list:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
```

## oberth uplink remove

```text
Usage of uplink remove:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -fingerprint string
        disambiguating SSH public-key fingerprint when one identity has several uplinks
```

## oberth uplink rotate

```text
Usage of uplink rotate:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -tls-cert string
        HTTPS certificate path passed to uplink add (default: the add default)
  -ttl string
        new bearer token lifetime (default: preserve the earliest existing expiry)
```

## oberth uplink scope

```text
Usage of uplink scope:
  -clear
        remove the limit: the uplink reaches every repository again
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -fingerprint string
        disambiguating SSH public-key fingerprint when one identity has several uplinks
  -set value
        limit the uplink to these upstreams (repeatable or comma-separated)
```

## oberth upstream

```text
Usage: oberth upstream <add|list|remove|provide-key> [flags]
Use oberth upstream <command> --help for command flags.
```

## oberth upstream add

```text
Usage of upstream add:
  -database string

  -dedicated-key
        provision a dedicated SSH identity for this upstream
  -expected-host-fingerprint string
        expected SSH host-key fingerprint (SHA256:...)
  -known-hosts string

  -known-hosts-secret string
        OpenBao known_hosts identity bundle (default "oberth-known-hosts")
  -namespace string
        Kubernetes namespace (default "oberth")
  -no-wait
        print the deploy key and exit immediately instead of waiting for its registration
  -upstream-key string

  -upstream-key-secret string
        OpenBao identity bundle holding upstream SSH keys (default "oberth-upstream-key")
  -yes
        auto-accept key generation and host-key prompts
```

## oberth upstream list

```text
Usage of upstream list:
  -check
        authenticate to each SSH upstream now and report whether the forge still accepts its key
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -known-hosts string
        upstream known_hosts (default "/run/oberth-identities/hosts/known_hosts")
  -upstream-key string
        upstream SSH private key (default "/run/oberth-identities/upstream/id_ed25519")
```

## oberth upstream provide-key

```text
Usage of upstream provide-key:
  -database string
        SQLite database path (default "/data/oberth.sqlite")
  -dedicated-key
        store as a per-upstream dedicated key
  -namespace string
        Kubernetes namespace (default "oberth")
  -upstream-key-secret string
        OpenBao identity bundle holding upstream SSH keys (default "oberth-upstream-key")
```

## oberth upstream remove

```text
Usage of upstream remove:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -yes
        skip the interactive confirmation
```

## oberth validate

```text
Usage of validate:
  -allow-unresolved-fragments
        deprecated: unresolved fragments still fail validation because admission requires their pinned contents
  -runner-image-prefixes string
        comma-separated allowlist of permitted runner image prefixes (default "golang:,debian:,aquasec/trivy:")
```

## oberth version

```text
Usage: oberth version [--schema]
Print build information, or the database schema version.
```

## oberth wait-idle

```text
Usage of wait-idle:
  -timeout duration
        maximum time to wait for the live daemon queue (in-pod) (default 2h0m0s)
  -wait-for string
        runs to wait for: all or critical (tag/promotion/plan/apply/unknown) (default "all")
```

## oberth upstream add (in-pod)

```text
Usage of upstream add:
  -database string
        SQLite database path (in-pod; requires the live admin daemon) (default "/data/oberth.sqlite")
  -dedicated-key
        provision a dedicated SSH identity for this upstream instead of the shared key (stored in the OpenBao upstream-key bundle)
  -expected-host-fingerprint string
        expected SSH host-key fingerprint (SHA256:...) for unattended bootstrap of unknown forges
  -known-hosts string
        upstream known_hosts (default "/run/oberth-identities/hosts/known_hosts")
  -known-hosts-secret string
        OpenBao known_hosts identity bundle (default "oberth-known-hosts")
  -namespace string
        Kubernetes namespace (default "oberth")
  -no-wait
        print the deploy key and exit immediately instead of waiting for its registration
  -upstream-key string
        upstream SSH private key (default "/run/oberth-identities/upstream/id_ed25519")
  -upstream-key-secret string
        OpenBao identity bundle holding upstream SSH keys (default "oberth-upstream-key")
  -yes
        auto-accept key generation and host-key prompts
```
