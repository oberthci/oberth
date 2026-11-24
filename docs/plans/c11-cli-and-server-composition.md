# C11: CLI and server composition

## Purpose

`cmd/oberth` is the executable boundary and composition root. It parses commands,
resolves configuration, calls component contracts, renders results, and owns process
startup and shutdown. It does not own admission, execution, publication, schedule
firing, database recovery policy, or installation planning. Kubernetes places Pods and
Argo executes Workflows. C11 supplies no capacity queue, dependency engine, workload
resource defaults, or alternate execution mode.

## Owned data and public contract

C11 owns `internal/client` (profiles, invitations, pinned transport and local
name resolution) and `internal/restartretry` (public-client timer waits). C9
owns in-step secret fetching and its execution wrapper. Runtime imports follow
[dependencies.json](dependencies.json).

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

C11 owns argument definitions, help, build identity, resolved command options, client
configuration selection, listener handles and task lifetime. It owns no run, grant,
issue, source-binding, schedule or audit table. Domain DTOs and their schemas come from
C1/C2; native document and plan types come from C3/C4/C5. Options are resolved once and
passed as typed values. Parsing does not apply changes or fetch credentials merely to
print help.

### Commands and output

C13 defines the happy-path limits. C11 supports `oberth promote <sha>` without
an input file, storing a secret without cluster access, pipeline
scaffolding and enrollment without hand-run openssl. Each uses the existing
service authority and admission boundary. Backups use age and an age backup key.

**Delta:** Every callable command and flag has a parity row or an explicit retirement. These
families define the responsibilities:

| Surface | Contract and owner called |
| --- | --- |
| `init`, `validate`, `check` | Scaffold native YAML; offline validation; shared live read-only plan check. C3/C4 own routing and admission. |
| `install`, `upgrade`, `rollback`, `unseal`, `backup` | C9's reviewed engine. Install has `--dry-run`, `--verify`, `--parallel`, `--no-pod-defaults`, `--no-link-local-filter` and `--external-store`, with no aliases. Install and upgrade take repeatable `--source-pin <consumer>=<producer>`, added to the saved source policy, and `--no-source-pins`, which drops the saved pairs first. Follow [C9's backup contract](c9-install-and-upgrade.md#backup) and [command inventory](c9-install-and-upgrade.md#owned-data-and-public-contract). |
| `secretstore setup`, `secretstore check`, `secretstore exec`, `secret put` | C9 store setup/checking and in-step fetch wrapper; the human `secret put` flow grants through C2, then writes. The underlying tool requires the grant. A known refusal before storage may say "not stored"; a lost reply follows the outcome-unknown rule below, for the grant and write alike. Private input uses stdin or a private file. |
| `repo add <forge URL>`, `invite <upstream>`, `join <file>`, `renew` | Register and verify the forge, bind this checkout, enroll local keys and renew the selected client certificate. An invitation grants every repository of that upstream; a repository path refuses with the upstream form. `join` saves the SSH endpoint from the verified enrollment reply in the existing profile field. C11 generates keys and requests. |
| `access allow <repo> <file> <step> <path>`, `access allow --kind pvc <repo> <claim>`, `access list/revoke/dismiss` | Exact secret and PVC grants; printed grant commands run as written. |
| **Delta:** `issue import <file> [--dry-run] [--upstream <local>]` | C2 validates registered upstreams and explicit mapping; C7 commits the bounded neutral manual-issue file atomically and preserves its numbers. |
| `release <tag>` | Verify fetched/local/tested commit and live release grants before creating the tag; tag, push and wait. A matching tag continues; a tag at another commit refuses. |
| `tool list`, `tool <name> [JSON\|-]` | Definitions and the raw declared result. Raw tool access remains available when a human flow owns the action. |
| `mcp --profile <uplink-profile>` | C11's stdio agent bridge with pinned profile transport. Without an explicit flag, only the checkout's local uplink binding selects credentials. |
| `serve --config <file>` | Start the daemon from one strict configuration file rendered by the chart. |
| `git-ssh`, `receive-hook` | Private transport and receive entries delegate to C6. |
| `internal-hold install`, `oberth-hold` basename | Private paired-helper entries delegate to C5 for install and run/status/exec/release. |
| `version` | `oberth <version> commit=<commit> date=<date>` plus newline; `version --schema` prints the schema integer. |

The command table and tool declarations define exact argument and flag spellings.
Each action has one human name. A tool-derived command path replaces underscores
with spaces only when no human flow already owns that action; a second name is
not installed. `tool <name>` remains the raw route.
Held steps offer list, arbitrary command inspection and release. Commands may
change files and outputs; the help says so. Native results stay authoritative.
Assist stays off under [C5 Assist enablement](c5-native-execution.md#assist-enablement).
`system status` reads installation health; `status <sha>` reads commit CI state.
Repository connection, promotion, release, audit and log flows use the CLI-owned
argument forms above rather than duplicating the tool table.

Top-level help renders callable commands from the catalogue in three groups;
it has no separately maintained command list. Usage errors name the offending
flag or argument and the correction. Validation reports all independently knowable offenders together. Output
write errors fail the command. Machine results go to stdout; diagnostics and progress go
to stderr. Commands with `-o json` emit the authorized server result without human
decoration.

Exit codes are 0 for successful command completion, 1 for operational refusal or
failure, 2 for usage, 3 when `repo add` registered the repository but the forge has not accepted its deploy key, and 130 for
interactive cancellation. A successful `run wait` observation exits 0 even for a failed
run; consumers inspect its result. Pending installation is not reported as complete.

**Delta:** Public JSON has explicit snake_case tags and collection envelopes, such as
`{"runs":[]}` and `{"issues":[],"next_before":123,"total":456}` when another page
exists. Run tool selectors use `id`. No alternate spelling is decoded. Preserve both
certificate flags through every remote wrapper.

### Client transport and checking

Every remote wrapper accepts `--url`, `--token-file`, `--ca-file`, `--client-cert`
and `--client-key`. Resolve flags, then environment, then saved client files.
URL, authority and identity are one binding. A CA override from `--ca-file` or
`OBERTH_CA_FILE` that differs from the selected profile's authority refuses before
any saved credential is used. URL and CA supplied without a profile use no saved
identity. Check endpoint identity, trust and key/certificate pairing before HTTPS;
credentials never travel over plain HTTP, including localhost, and there is no
insecure flag. Explicit invalid credentials never fall back to another identity.

MCP requests use JSON-RPC `tools/call` with `{name,arguments}` and support JSON and SSE
replies. C1 owns exact request keys, schemas, timeouts and mutation classification. C11
consumes those definitions rather than maintaining a second decoder or authority map.
Reconnect only operations declared safe reads, within the original deadline. Unknown
tools and mutations never replay after a lost reply or restart. A lost reply or
transport failure after a mutation left the client reports "outcome unknown".
Inspect durable state instead. For `secret put`, check `secret.put.requested` in
the audit; putting the value again is safe, but the request audit is not proof
of storage. Check the grant separately when its reply was lost. Observation
cancellation never cancels a run. `run wait` sends `run_wait
{id,until,timeout_s?}` and renders C2's `state`, `run`, `holds`, `waited_s` and optional
`retry_hint`. `events wait --after-id` sends `after_id` to `events_wait`;
filtering, retention gaps and the next cursor remain C2's contract.

`validate [file]` uses canonical repository bindings and reports its offline scope; deferred template
references and live facts point to `oberth check`, without claiming missing authorization. **Delta:** add
`check [ref] [--repo <canonical-repository>] [--local <path>] [-o json]`, calling
`run_check`; it creates no run, Workflow, token or grant declaration. It shares C3/C4's
routing and prepared admission path, reports identity, grants, source, authored
storage/deadlines and enabled optional policy, and preserves unknown observations. It
makes no scheduling or resource prediction. Use the same shared plan model for offline
and live checks. `init` authors source input and Pod-local scratch and writes the
portable source-trust setting explicitly, commented as redundant with C5’s automatic
hook. Examples and the migration guide show the same setting explicitly.

`oberth renew` renews the selected profile's client certificate through C2's
enrollment contract; C11 verifies and saves the matching credential before use.
The bridge forwards the session's pushed event stream and resumes it from its
last event ID; durable `events_wait` and per-request progress remain available.

### Repository and identity choices

**Delta:** `repo add` accepts normal SSH, SCP-style SSH and HTTPS forge addresses with the
forge's own SSH user, a path of any depth and an optional `.git` suffix. As in Git, a relative SCP-style path of a user other than `git` is in that user's home and is stored as `ssh://user@host/~/path`; an absolute SCP path and a `git@` SCP path keep the root form. A new upstream
stores the address without its last part; that namespace's last directory is the organization.
The user and every path part are letters, digits and `._-`, start alphanumeric and hold no
`..`, and only a leading `~` names the home; passwords and tokens are refused, and an address without a user logs in as `git`.
Existing upstream addresses retain their stored scheme, user, host, port and namespace and
match only an address with the same user. Discovery is a live scoped read: a joined caller
binds exactly one visible registered match without an admin call, names ambiguous candidates
and prints a usable administrator command when the repository is unavailable. `--upstream` selects among visible matching addresses; it never turns the supplied address into only a path selector. An empty readable forge repository binds normally; an administrator seeds its default branch through the Oberth remote, using ordinary admission, CI and publication checks. Before the first publication, an administrator may advance the accepted default head through the same fast-forward and CI checks, including to fix a failed first run. Receiving without a cached default head requires a live empty-forge result; a non-empty forge does not excuse that missing head.

**Delta:** Binding uses a per-remote `file:///` address and pack commands on the
dedicated `oberth` remote. Its pack commands use the server SSH endpoint saved
by join, pinned by the Oberth server's SSH host keys that the selected profile saved at install or join (`--known-hosts` replaces them); without them `repo add` refuses and names `--known-hosts`, and join asks for a new invitation, and `~/.ssh/known_hosts` is never read. The forge's key is `--expected-host-fingerprint`, pinned by the server. `repo add` checks Git's effective fetch and push addresses and transport
policy; a rewrite or policy that prevents the intended binding refuses with a
remedy. It replaces or clears only configuration that `repo add` itself wrote,
including its obsolete push URLs; `repo add` and join clear obsolete `oberth.knownHosts` settings, and leave a person's settings and every
other remote unchanged. An administrator binding clears only the product's
stale uplink binding.

Browser-only identities need no SSH key; generate only the keys required by
the chosen connection.

Public release consumers and publication use one canonical public release origin.
Downloads follow at most three HTTPS redirects, under it or to GitHub's asset hosts; a refused target shows as separate detail. Offline
inputs follow C9's [Release verification](c9-install-and-upgrade.md#invariants).

### Health, readiness and metrics

C11 owns health/readiness snapshots, metrics and scoped diagnostics, including
database, cluster_detail, upstream_info, secret_store, audit_chain and
schema_version fields. Background readiness samples every 5 seconds, probes
within 30 seconds and retains a last success for at most 90 seconds. Deadline
or typed reader contention may retain green without renewing its age; a hard
writer failure or incomplete recovery refuses readiness. Requests read snapshots.
Cluster, audit and login diagnostics do not gate readiness. Diagnostic intervals
are fixed: one sampler every five seconds. Watch synchronization waits on the
server context. C11 owns samplers and
joins them before closing dependencies.

### Daemon composition and local authority

`serve --config <file>` reads one strict configuration file, not a second
policy model. The chart renders component-owned sections. Report unknown,
missing and invalid settings together. Validate configuration
before starting application work. Run exactly one server process; service ref
locks, scan slots and retry timers assume one replica. Read the schema/audit identity once and pass
it to every store opener; never infer it from stored rows. The daemon delegates audit
verification and schema decisions to C7 and its audit adapter. It opens existing state
for writing only after schema and local audit inspection. Fresh genesis cannot
overwrite existing evidence. Daemon startup performs no old-layout migration. Cutover is an operator procedure outside the product.

Composition order is: resolve configuration and identity; inspect/open state checked for
schema and local audit integrity; construct adapters and required mutation gates;
construct one shared Workflow/Pod watch; synchronize it; recover owners and accepted
receives with their audit records; activate actions; after readiness, run the startup schedule reconciliation in the background (`oberth_schedules_synced` reports its end). Wire
on-demand declarations, grant publication, source recovery and transactional
events to their component owners. No missing security dependency silently disables a gate.

Bind SSH, API HTTPS and source HTTPS separately. Defaults are `:2222`, `:8443` and
`:8445`; source exposes only the source route and shares the renewing TLS identity. C1
owns application handlers, authentication and probes. Bind failure closes
already-created sockets. The API listener binds first with the real identity;
until startup completes, every route except `/livez` and `/readyz` answers 503.
Identities are mounted files; there is no startup identity retry.

The server's own state directory follows server custody rules, separate from
personal profiles: its keys remain private and its directory stays usable across
Pod replacement under the chart's security context. Personal-profile reads retain
their strict checks.

The server reads the install administrator's public key from one Secret at start
and passes it to [C2's InstallAdmin contract](c2-service-actions-and-history.md#administrator-only-actions).
C2 exports its typed tool bindings for composition. Idle, readiness and version are administrator
tools; there is no local maintenance socket or install-time server exec.

The server issues its leaf from the CA Secret and reissues it at two thirds
of its lifetime. Readiness requires a currently valid leaf.

**Delta:** The server starts each part's background work once and stops parts in reverse
order after HTTP drain. Missing parts or settings refuse startup. Composition
calls store retention once a day using [C2's event-retention rule](c2-service-actions-and-history.md#events-and-observation).

Shutdown withdraws readiness and begins HTTP drain before canceling task owners. Stop
accepting SSH work, drain C3 Close and accepted submissions, then cancel
and join watchers, samplers, renewal, reconciliation
and action tasks, drain observers, stop service background work, then close stores.
Running Workflows remain for verified restart adoption. Normal shutdown does not stop,
delete or resubmit them. C2/C5 own explicit cancel semantics. Preserve cleanup errors
and cancellation causes.

**Delta:** Version/status outputs retain their declared formats. Bind version,
commit, date and schema to the selected build and database, so identity values
vary by installation. Health, readiness and metrics belong to C11; retain
readiness verdicts and scenario counters, with typed DTO and metric names as
declared here.

**Delta:** The happy path allows `oberth promote <sha>` without a file,
pipeline scaffolding, secret storage without cluster access and enrollment
without hand-run openssl. Each command uses the same service authority and
admission checks as other clients.

## Allowed dependencies

C11 is the only composition root. Its exact local allowlist is in
[dependencies.json](dependencies.json). It may construct C1–C10, inject their ports and
use the named shared helpers and native clients. No component imports cmd/oberth; C11
imports no C12 and no C13 harness or pipeline implementation. Its two C13-owned imports
are `internal/releaseverify` (bounded client) and `internal/staticgit` (pure packaging),
per `dependencies.json`. Runtime values come from the one strict server configuration; C9
installer imports serve installation commands only. No domain policy is copied into
wiring.

## Invariants and budgets

- Pipeline namespace differs from server namespace; execution identities stay distinct.
  Required native configuration and plugin registration fail closed. Argo is required.
- [C9 invariant 7](c9-install-and-upgrade.md#invariants) owns the policy floor,
  egress and review defaults. C11 passes explicit choices through.
  There is no CPU math, disk gate or application run cap; Argo owns the global cap.
- Local audit always runs. Local chain checks run on reads and backup.
  Mutations atomically save their audit record without chain verification.
- Operation deadlines include child I/O, reads and waits. Mounted identities
  need no startup retry. Liveness fails if the sampler makes no progress for
  five minutes. Thread the owner context through startup and registration.
- MCP defaults to 60 seconds or tool timeout plus 30 seconds, whichever is longer; an
  explicit request deadline and an earlier caller deadline win. `run wait` is at most
  600 seconds server-side. Reconnect shrinks the next long poll to remaining time, never
  renews it.
- HTTP shutdown has a shared 15-second context; service shutdown gets 10 seconds. Every
  worker needs cancellation and a positive join before store close. Do not close
  dependencies under a live task.
- Idle observation has a five-second read budget, one-second interval and ten-second
  freshness limit measured from read start. Missing/stale is unavailable, never idle.
  Upgrade shows in-flight runs without waiting or freezing admission.
- C8 owns the 64 MiB default retained run-log bound; C7/C9 own backup default 1 GiB and
  hard 4 GiB database bounds. Pass limits through without an unlimited fallback. Local
  check inputs are 1–32 files, 1 MiB total. Native spec and plan bounds come from C4/C5.
- Author files are bounded regular files confined to the repository; refuse symlinks.
  Scaffolding refuses overwrite without `--force` and preserves sibling files. Secrets
  never enter argv, ordinary logs or result JSON. C11 dispatches
  `oberth secretstore exec` to C9's step wrapper with private memory scratch,
  owner-only directories/files and bounded fetches.

`oberth promote <sha> --repo <repository>` uses C2 and the server's retained tested commit without a local checkout and
prints the tested run, resulting main commit and CI run at that commit. Client
credential writes use an owned directory that other users cannot write, owner-only
files, and cleanup of partial plaintext.

## Inspections

Composition inspections exercise push-time schedule synchronization through the
ordinary push path; no untested composition branch is exempt from the check.

The requirement sentence is the inspection contract. These inspections are planned.

| Requirement | Inspection |
| --- | --- |
| [REQ-C11-001](REQUIREMENTS.md#req-c11-001) | `TestCompleteAuditTailContract` (contract_test) |
| [REQ-C11-002](REQUIREMENTS.md#req-c11-002) | `connect` (walk_step) |
| [REQ-C11-007](REQUIREMENTS.md#req-c11-007) | `TestOwnedPrivateCliFilesContract` (contract_test) |
| [REQ-C11-012](REQUIREMENTS.md#req-c11-012) | `TestOneTimeBearerActivationContract` (contract_test) |
| [REQ-C11-013](REQUIREMENTS.md#req-c11-013) | `TestInstallationBoundClientAuthorityContract` (contract_test) |
| [REQ-C11-021](REQUIREMENTS.md#req-c11-021) | `TestHonestResponsiveHealthContract` (contract_test) |
| [REQ-C11-022](REQUIREMENTS.md#req-c11-022) | `connect` (walk_step) |
| [REQ-C11-023](REQUIREMENTS.md#req-c11-023) | `connect` (walk_step) |
| [REQ-C11-024](REQUIREMENTS.md#req-c11-024) | `promote` (walk_step) |
| [REQ-C11-025](REQUIREMENTS.md#req-c11-025) | `TestServerShutdownContract` (contract_test) |
| [REQ-C11-026](REQUIREMENTS.md#req-c11-026) | `connect` (walk_step) |
| [REQ-C11-027](REQUIREMENTS.md#req-c11-027) | `TestServedLeafRenews` (contract_test) |
| [REQ-C11-028](REQUIREMENTS.md#req-c11-028) | `connect` (walk_step) |

**Delta:** `connect` checks generated keys, matching invitation certificates,
occupied-path refusal before redemption, a consumed-invitation remedy, selected
identity/scope/profile and repository binding, `.local` discovery with TLS still
bound to the original name, repo registration/forge acceptance and the smoke push
after a fresh join using the returned SSH endpoint.
It also checks the bridge's display block, complete reasons and separate detail,
all accepted repository-address forms, credential-bearing and prefixed-path
refusals, exact preservation of other remotes and browser-only enrollment without
an SSH key.
`TestBridgeDisplayBlock` tests this over the protocol. `TestServedLeafRenews`
checks renewal at two thirds and the served CA chain. `promote` invokes `oberth promote
<sha>` without a file and checks the tested run, resulting main commit and new CI run.
`TestRemoteTransportFlags` exercises every wrapper with explicit client certificate/key
and trust flags. `TestServerShutdownContract` blocks each dependency and proves
cancellation, reverse-order join and no duplicate native work.
`TestRefuseIncompleteComposition` checks every missing part and required setting. The health inspection separates local
liveness from external waits and checks the 90-second database readiness bound.


Configured server identities use one bounded regular-target mount reader; projected
symlinks and replacement targets are accepted. Person-owned profile files retain
their own stricter reader. Forge and host keys parse before storage or listeners
open; Git is an explicit executable regular file. Listen ports are numeric 1–65535.
The chart supplies runtime defaults; the loader requires every runtime setting.
C2 supplies all three HTTP views through its existing authority and log owners;
a missing view refuses startup. Constructors receive startup cancellation, then
retain the detached owner lifetime after construction. Wiring and native workers
join before service shutdown; an unfinished owner keeps its dependencies open.

## Retired terms

Wire aliases → explicit DTOs; engine fallback → native Argo; startup migration →
operator cutover outside the product.
