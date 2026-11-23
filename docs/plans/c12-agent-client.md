# C12: Agent client

## Purpose

C12 is `agent/`: a shared instruction file, a POSIX verified fetcher and optional
host plugins connecting an agent to the public CLI and MCP contract. It guides
install, repository setup, push, wait, inspect, promote and release. Admission,
authority, profiles, execution
and credential policy belong to their component owners.

## Owned data and public contract

**Delta:** `agent/skills/oberth/SKILL.md` contains the complete guidance in fewer
than 100 lines. `agent/skills/oberth/fetch-oberth` verifies and installs the CLI.
C13's [release trust](c13-own-pipelines.md#release-trust) owns the canonical key
and provisioning. The standalone fetcher embeds a generated copy and its hash
because it runs before a checkout exists.
`make check` runs C12's fetch contract, which proves the canonical key, embedded
key and hash agree.

The optional plugin under `agent/` supplies the run box and shared skill through
host manifests. It adds no CA trust edit, deploy-key upload or credential store.
C11 owns profiles, keys,
the current-profile link, invitations, repository binding and renewal. C1 keeps
the server agent-instruction constant and its test; C12 supplies its wording.

| Work | Public path |
| --- | --- |
| Install | Fetch a verified CLI when needed; show `oberth install --dry-run` whole, then apply with `--yes` after approval. Plain prompts end with a review screen. Preserve the chosen context and print the result and next action unchanged. |
| Connect | `oberth join <invitation-file>` prints the absolute executable and `mcp --profile <uplink-profile>` registration argv. Register exactly that line. The host does not need the installation CA or credential paths. |
| Repository | `oberth repo add <forge URL>` registers, verifies the forge's deploy key and binds the checkout. The person adds the printed deploy key at the forge. Use `oberth init` when needed, get approval for the commit and push the reviewed commit with `-o oberth.run=1`. |
| Recovery | After reboot, run `oberth unseal` without arguments; the selected profile supplies the private recovery path. Never read or print its contents. |
| Observation | Paste the bridge's display block or `oberth run list` / `oberth run get <run>` output verbatim. The web link supplies a live screen. |

**Delta:** `oberth mcp` is the authenticated model-tool connection; optional
display plugins read public CLI JSON. The bridge also advertises Codex's
status-line entrypoint on `run_list`, serving `ui://oberth/runs` locally. It reads
`run_list(limit: 3)` as the selected joined uplink and caches results and public
refusals for 15 seconds. Only a host read starts a stale refresh; pending reads
coalesce. Reads answer with a tree or placeholder within one second; EOF cancels
refreshes. C12 owns this display and its Go renderer in
`internal/client/bridge_statusline.go`, the second implementation of the run-box
cells in `agent/hooks/model.ts`; changes to those cells check both renderers.
C11 owns the stdio bridge and its pinned installation transport. Registration explicitly
selects a joined uplink profile; without that flag only the checkout's local
uplink binding selects credentials. Missing selection, admin profiles and
unclassified profiles refuse before credential loading, stdin or network use,
with a join-first remedy. Install asks the person to join an uplink. The host's
system trust and settings do not change.

### Run box

**Delta:** The optional box shows up to `runs` recent runs (three when unset or blank,
none at 0) while a listed or watched run is active or finished in the last ten minutes of
the server's clock that the run list carries, with watched runs first in any state. Each
run is one line under a header: id, repository and ref in short forms, status word,
authoritative plan counts, duration, age, a watched mark and a failed run's step name,
measured in terminal cells, cut to the box's width and never wrapped; the last column
gives way first. `/oberth-runs` prints the same lines with a failed run's step and reason
or a cancelled run's reason last; there the names give way before the reason.
Finished watches produce one completion notice. The box, `/oberth-runs` and `/oberth-watch`
read only public `run list --json` and `run get --json`, through the absolute verified
executable (its option, by default the fetcher's install in the user bin directory),
never a checkout executable or a PATH lookup. The plugin reads with the agent's joined
uplink profile, selected by its option, `OBERTH_PROFILE` or the checkout's joined uplink
binding. A nonblank `profiles` option lists up to 32 comma-separated joined profiles
instead; reads merge newest first, deduplicate run IDs and apply one total row limit.
Each profile list keeps its server-clock offset even without rows; unlisted watches
use that offset and keep their source profile. Failed profile
reads hide that profile's stale rows while healthy profiles remain visible. When every
profile read fails, the box hides. Without a selection it explains setup once and stays hidden.
It registers no model tools: they would share the bridge's server name, and the bridge serves the model. It reads no credential paths, server state or cluster API.

One timer refreshes every 15 seconds, active or idle, so short runs and terminal pushes
are still seen. Refreshes coalesce and each CLI call is bounded. Session start does not
await the first refresh. Missing configuration is explained once per installation;
unreachable or malformed responses hide stale rows quietly. Session exit cancels the
timer.

Plugin packaging follows C13's [guard naming policy](c13-own-pipelines.md#check-tooling).
The connection skill stays shared and host-neutral.

### Push and wait

The server's MCP instructions carry the loop once. With work in flight, keep
one background `events_wait` open for its held/finished events and add promotion completion
while promoting. Before pushing, call `events_wait` with `snapshot:true` and no `after_id`;
it returns a cursor immediately. Push, then wait from that cursor, even if background calls are unavailable. Handle every event, deduplicate IDs and advance
`next_after_id`, including after timeout; a gap requires a new snapshot. Pushed
notifications are an extra. A parked push has no run; request CI with a real ref
update and `oberth.run=1`.

Read a finished run; if cancelled, show who cancelled and stop without fixing code or rerunning. Otherwise use its typed retry hint. A rerun hint permits unchanged
retry. For none, read the failed step's logs and retry_hint.detail; fix a shown code fault and commit/push with consent, or show the reason, detail and log lines to the person and stop. Assist remains
off pending review and real-cluster checks. On run.held, list, inspect with exec,
push the authored fix and release. There is no continue or file tool. Commands
run with the step identity and mounts and may change files and downstream
outputs; the help says so. Never replay an exec after an uncertain reply.
Native results are authoritative; the original code is best-effort diagnostic
text. Eligibility excludes declared and resolved secret routes, without
certifying image contents, fetched values or readable shared state. Promote
only a passing run for the exact commit with neither an audited inspection exec
nor an armed step without a succeeded final native result. Merely
requesting assist leaves evidence ordinary; a command audit marks the run before
dispatch, even if that dispatch fails. Finish also marks a run when any sealed armed step did not succeed, including a
missing native result. Armed steps that all succeeded stay ordinary unless
inspected. List and release do not themselves mark it.
Server counts are authoritative.
`object_sha` names the accepted object; `commit_sha` names the tested commit.
A lost mutation reply requires reading durable state before another action.

## Allowed dependencies

C12 has no server imports. [dependencies.json](dependencies.json) grants only
its test permissions. The instruction file and POSIX script call the public CLI,
Git and signature/digest tools. Optional host hooks use the same public CLI JSON,
the checkout's joined uplink key in its Git configuration, and the host's state,
clock and rendering APIs. They read no server database,
credential files or Pod environment.

## Invariants and budgets

1. MCP registration names the absolute verified CLI, never a bare executable name
   or project file. No C12 display code spawns a CLI from the current directory.
2. Fetch bounds VERSION at 128 bytes, checksum manifest at 8192 bytes and binary
   at 512 MiB while receiving. Verify the pinned public-key hash and signature
   bundle under C13's [key-only release trust](c13-own-pipelines.md#release-trust),
   safe version/artifact names and matching binary digest before publication.
   Serialize by destination, stage beside the executable and rename atomically;
   a failed update preserves the working executable and removes partial bytes.
3. Secrets, invitations and recovery material enter commands by private-file path;
   the agent never reads or prints them. Reuse approval for the same action. Show
   the concrete target and changes before an unapproved install, commit, push,
   promotion or release. Do not change global Git/SSH configuration or run server
   Pod commands. Report observed facts only.
4. Run results (`run_get`, `run_wait`, `wait`, and `run_list` as its table) carry the
   server JSON followed by C11's rendered display block on its own line, both as text
   content: a host given structured content shows the model only that, so the bridge
   lists no output schemas. Status and event pages carry their JSON alone. Paste the
   block verbatim with whole reasons and separate native detail. Strip terminal
   controls and format marks; do not infer an error class from prose or clip text.
5. The bridge forwards pushed session events with cursor resumption and maps
   keepalives to protocol progress for requests with a progress
   token. Hosts with smaller tool timeouts pass a smaller `timeout_s`. A long
   event wait cannot block run reads; cancellation ends observation only.
6. The instruction file names existing commands and tools, uses neutral wording,
   and is read whole in fewer than 100 lines. The first-run review adds one command
   and one flag to the person's path; C13's command and flag ceilings apply.

## Inspections

| Requirement | Inspection |
| --- | --- |
| [REQ-C12-001](REQUIREMENTS.md#req-c12-001) | `TestAgentRegistrationContract` (contract_test) |
| [REQ-C12-003](REQUIREMENTS.md#req-c12-003) | `TestAtomicCliInstallationContract` (contract_test) |
| [REQ-C12-007](REQUIREMENTS.md#req-c12-007) | `ux-gate` (walk_step) |
| [REQ-C12-008](REQUIREMENTS.md#req-c12-008) | `ux-gate` (walk_step); `TestRunBoxContract` (contract companion) |

`TestAtomicCliInstallationContract` must cover invariant 2's key-only release
verification as well as serialized, atomic installation and preservation of
the working executable on failure.

The plugin contract check binds every CLI command, flag and JSON field to the
public catalogue and DTOs and refuses credential/home paths. Rendering remains
a manual host-runtime gate; rendered trees also check the supported host's prop names,
with the exact command beside the rendering tests;
the build image has no host JavaScript runtime. The UX walk checks active,
finished, unreachable and watched states, unwrapped lines, profile selection, idle
and active call counts, nonblocking startup and a single completion notice.

`TestAgentRegistrationContract` checks command/tool names, registration rules
and the instruction line count. Fetch tests refuse a changed key, signature,
manifest or binary, serialize updates and preserve the working executable on
failure. Agent-owned tests check the instruction file's absolute/verified
registration rule and the fetcher's publication. C11 separately checks its printed registration,
profile selection, pinned transport and bridge display. The walk
executes the guidance through install review, join, repo binding, a real source
push, wait, promotion and release, without host trust changes.
