# Agent-assisted CI continuation (retry hold)

A branch CI step fails. Today the run is red, the agent reads the logs,
guesses, pushes, and waits for a cold rebuild. With assisted continuation the
agent instead enters a live copy of the failed step's environment, fixes the
files the step reads, re-runs the step in place, and lets the workflow finish
natively. The result is a diagnosis, never a verdict: an assisted run is not
evidence for anything. Promotion, publication and "green for SHA" still need
an ordinary, unassisted run of a real commit.

Off by default (`assist.enabled: false`). Branch CI only. Credential-free
steps only. Opt-in per run.

## How a step is held

Oberth runs pipelines on Argo Workflows v4.0.8 with the emissary executor.
Two facts of that executor make a hold possible without any change to the
repository's YAML, to Argo, or to the step image:

- When the main container has `ARGO_DEBUG_PAUSE_BEFORE=true`, the executor
  (PID 1 of the main container) waits before `exec`ing the step command until
  the file `/var/run/argo/ctr/main/before` exists
  (`cmd/argoexec/commands/emissary.go`).
- The controller drops the Pod `activeDeadlineSeconds` only when that
  variable is on the *template's* container; it checks before it applies the
  template `podSpecPatch` (`workflow/controller/workflowpod.go`). A
  `podSpecPatch` is substituted with the retry-local variables `retries` and
  `lastRetry.exitCode` right before each attempt's Pod is created
  (`workflow/controller/operator.go`, `SubstituteParams`).

For a run in assist mode, `Build` adds to every eligible pod leaf:

- a server-owned `retryStrategy`: `limit: 1`, `retryPolicy: OnFailure`,
  `expression: asInt(lastRetry.exitCode) > 0`. Only a command that exited
  nonzero is retried. A Pod killed at its deadline, an eviction or an executor
  error has no exit code (Argo substitutes `-1`), so attempt 1 is classified
  exactly as before (#1499 start-up deadlines included) and the run-level
  infrastructure retry is unchanged. A leaf with its own `retryStrategy` keeps
  it and is not held;
- a server-owned conditional `podSpecPatch`, an Argo expression template
  `{{= asInt(retries) > 0 && asInt(lastRetry.exitCode) > 0 ? '<held>' :
  '<base>' }}`. `<base>` is the plain server patch every step already gets.
  `<held>` additionally sets `ARGO_DEBUG_PAUSE_BEFORE=true` and
  `OBERTH_ASSIST_HOLD=1` on `main`, sets the Pod `activeDeadlineSeconds` to
  the step's #1499-floored budget plus `assist.holdTTL` so the kubelet bounds
  the whole attempt (hold and the command after release) while the server
  abandons an unreleased hold once `holdTTL` has passed, makes the `/work/src` mount writable, makes the shared
  `/work/cache` mount read-only and moves every cache variable that pointed
  into it (`GOCACHE`, `GOMODCACHE`, `OBERTH_CACHE_DIR`) to
  `/tmp/oberth-assist-cache`, and pins the server's architecture when the
  server helper is mounted. Attempt 1 gets `<base>`: its deadline, mounts
  and caches are exactly today's;
- the template annotation `oberth.ci/assist-hold: <held deadline seconds>`,
  inherited by the Pods, and the mount of the server's own executable at
  `/run/oberth/usage/oberth` (the mount measured steps already have) so
  `run_hold_patch` and `run_continue` need no shell in the image.

The Workflow gets `oberth.ci/assist: <hold TTL seconds>` and its
`activeDeadlineSeconds` grows by one hold TTL, so a parked step is not cut
short by the run's own budget; the executor token lifetime and the server
watchdog follow the Workflow deadline. Both annotations are server-owned:
template-metadata admission refuses `oberth.ci/assist-hold` from
repositories and `Build` deletes an authored `oberth.ci/assist`.

Attempt 1 runs immediately. If its command exits nonzero, Argo creates the
retry Pod with the held patch: the executor parks before the command, the
step shows as running, and the run keeps its workflow slot, exclusive
classes and concurrency group. If nobody acts before `holdTTL`, the server's hold sweep (the scheduler's
settlement tick) abandons the Pod, the retry is exhausted and the run fails
as it would have; the kubelet deadline (budget plus TTL) is the backstop if
the server is down. That deadline is not the recorded floored step deadline,
so #1499 never classifies a timed-out hold as a start-up failure, and a
retained failed step prevents the automatic infrastructure retry.

## Opting in

- `git push -o oberth.assist=1` (or `=true`) on a branch. The option is
  recorded with the receive reservation like `oberth.priority`, survives
  replay, and becomes the run's immutable `assist_requested`.
- `run_rerun` with `assist: true`, or `oberth run rerun <id> --assist`.

The request is honoured only when all of these hold: `assist.enabled`, the
run is branch-tier CI started by a branch push or its rerun (never release,
tag, promotion, schedule, plan or apply), and the workflow declares no
secret-store paths. Otherwise `Build` injects nothing and the run behaves
exactly as today.

## Eligible steps

A leaf is held only when it has no credentials and no identity of its own:

- a single-main `container` or `script` template (no DAG, steps,
  ContainerSet, resource, suspend, data, HTTP or plugin template);
- no authored `retryStrategy`, and no `templateDefaults.retryStrategy` in
  the document (an injected strategy would silently replace the default, so
  such a document never holds);
- no `oberth secretstore exec` or envconsul command, no
  `oberth.ci/release-wif-role`, no per-step `serviceAccountName`, no
  `automountServiceAccountToken: true`, no token, secret or WIF volume
  mount;
- not a declared nonroot launcher (`oberth.ci/nonroot-templates`): those
  lanes, such as Oberth's own static-git qualification, forbid exec and own
  their Pod patch.

Everything else keeps today's behaviour. Steps whose image lane lacks the
server helper mount are inspect-only: `run_hold_exec` works, `run_hold_patch`
is refused.

A hold is identified by the server-owned annotation, the pause variable on
`main` and a retry node name (`...(N)`, N ≥ 1); admission refuses every
`ARGO_*` environment name from repository YAML, template defaults and
fragments included, so a document cannot pause its own attempt 1 (which
Argo would run without a Pod deadline) or impersonate a hold.

## Working a hold

MCP tools (CLI: `oberth run hold ...`):

| Tool | Does |
|---|---|
| `run_hold_list` | Parked attempts of the run: step, pod, attempt, held_since, kubelet deadline, `patch_capable`, `released`; plus the assist record and the installation policy. Read-only. |
| `run_hold_exec` | Run one `argv` inside the held `main` container. No shell: the server passes argv straight to `pods/exec`. Bounded by `assist.execTimeout` and `assist.execOutputBytes`, optional `stdin` (1 MiB), optional shorter `timeout_seconds`. |
| `run_hold_patch` | Apply a unified `diff` to `/work/src`. The server parses it (plain text file patches only: no renames, copies, mode changes, binaries, `.git`, absolute or parent paths), reads the touched files through the Pod helper, applies every hunk with exact context (no fuzz), and writes them back with the read digests as preconditions; the helper refuses the whole write if a file changed in between, never follows a symlink, and writes through a same-directory temporary and rename. The diff's sha256, size and file list are recorded. |
| `run_continue` | Create the release marker; the executor runs the real command, Argo records the attempt and continues the DAG on success. |
| `run_hold_abandon` | Delete the held Pod; the step fails normally and the run ends red. |

Rules shared by every mutation: the caller is the run's own uplink
(`run.actor`) or an admin; a scoped uplink sees only its repositories' runs;
the run must be active; actions are serialized per run and capped
installation-wide (`assist.maxConcurrentHolds` slots); each one is appended
to the run's `assist_record` and audited as `run.assist.<kind>` with actor,
run, step, pod, argv and exit code or patch digest, bytes and files.

Every exec, patch and continue is recorded **before** it executes, so the
run is assisted from the first action whatever it does: an exec may edit the
tree or create the release marker itself, and a patch may be half-applied.
If creating the marker fails, the Pod is abandoned and the step fails: a hold
is never left in an unknown state. `assist.execTimeout` stops the exec stream
and returns `timed_out`; a process the image keeps running after that is not
killed, which is one more reason an exec'd run is not evidence.

`run_hold_exec` is a diagnostic surface, not an editing one: the Pod has no
shell guarantee and the exec runs as the step's user inside the step's
cgroup. The helper executable is the server's own static binary; its start-up
footprint is the same the measured-step wrapper documents.

## Evidence

**A run with any exec, patch or continue on a held step is assisted**
(`runs.assisted`, monotonic, set when the action is recorded). It is excluded
from:

- promotion sources (`greenBranchCandidate`, single promotions, trains and
  retries);
- upstream publication: the run finishes `passed` in Oberth but
  `beginRunPublication` is skipped, so the branch is never force-synced from
  a tree that is not in Git;
- SHA-pinned fragment evidence (`HasPassedRunForSHA`);
- CI-issue resolution and issue-trailer closing.

No status, label or layout is added. The reason line of the existing status
says why: `assisted: step test continued after patch sha256:ab12cd34ef56 by
uplink-a; not evidence`. The record keeps every patch digest, so a later
commit that carries the same change can be matched by reading the run.

**A hold continued with no patch and no exec is still not evidence.** It is
tempting to call that a plain retry, equal to an authored `retryStrategy`
attempt. It is not equal: the held attempt ran with a writable source mount
and detached caches, in a Pod an agent could reach, after a pause of up to
`holdTTL`. Nothing in the run proves that nothing touched the tree. The
conservative rule costs one ordinary push and keeps the evidence model
simple: *assisted means not evidence, always*. The reason line distinguishes
the cases (`continued after patch`, `continued after exec`, `continued
without changes`) for humans.

A run that asked for assistance but never held (every step passed, or a
failure was not hold-eligible) is ordinary evidence; the opt-in alone changes
nothing.

## Caches

The shared `/work/cache` (Go module and build caches, per repository and
tier) is read-only on a held attempt and every cache variable that pointed
into it is moved to private scratch, so a patched tree cannot write into
state later runs read. Attempt 1 uses the caches exactly as before. A held
attempt therefore starts with cold caches; that is the price of the shared
cache's integrity, and it is bounded by the step's own budget plus the hold
TTL. Per-run workspace caches (`work` volume claim templates) are
per-workflow and unaffected.

## Capacity, supersession and bounds

- A held run keeps its workflow slot, exclusive classes and concurrency
  group until its Workflow ends: it counts as running work.
- The scheduler's settlement tick sweeps holds: an unreleased hold older
  than `assist.holdTTL` is abandoned, then the newest parked holds above
  `assist.maxConcurrentHolds` are abandoned. Holds whose run's assist record
  shows a continue or abandon are running commands and are skipped; if that
  record cannot be read the sweep does nothing rather than kill a released
  step.
- A newer push that supersedes the run deletes its Workflow through the
  existing cancellation path, which removes any held Pod; explicit
  `run_cancel` does the same.
- Hold operations run only in the server's own pipeline namespace through
  the `pods/exec` and `pods` delete verbs the chart's Argo Role already
  grants; no RBAC is added. Runs placed on remote execution targets are not
  reachable by the hold tools in this release.

## Helm values (serve flags; chart default equals binary default)

| Value | Flag | Default | Bound |
|---|---|---|---|
| `assist.enabled` | `--assist-enabled` | `false` | |
| `assist.holdTTL` | `--assist-hold-ttl` | `20m` | whole seconds, at most `4h`; unreleased holds are abandoned after it, the held Pod deadline is step budget + TTL |
| `assist.maxConcurrentHolds` | `--assist-max-concurrent-holds` | `2` | 1-64 |
| `assist.execTimeout` | `--assist-exec-timeout` | `1m` | at most `15m` |
| `assist.execOutputBytes` | `--assist-exec-output-bytes` | `65536` | at most 4 MiB |
| `assist.patchMaxBytes` | `--assist-patch-max-bytes` | `1048576` | at most 16 MiB; also the largest patched file |
| `assist.patchMaxFiles` | `--assist-patch-max-files` | `64` | at most 1024 |

## Interaction with other policies

- **#1499 start-up allowance**: attempt 1 keeps its floored deadline and the
  recorded annotations; the held attempt's deadline differs from the recorded
  floor, so its expiry is the step's own failure, never a start-up
  infrastructure attempt. The injected `retryStrategy` never retries a
  deadline kill, so a start-up failure does not get a second in-workflow
  attempt before the run-level infrastructure retry sees it.
- **Workflow deadline**: extended by one hold TTL per assisted run.
- **Exclusive classes, concurrency groups, admission hold**: unchanged; a
  held run is a member for the whole hold.
- **Adoption after restart**: the Workflow carries everything the hold tools
  need (`oberth.ci/assist`, Pod annotations and env); the assist record is
  durable in the run.

## Schema

Migration `assisted-runs` adds `runs.assist_requested` (immutable),
`runs.assisted` (monotonic) and `runs.assist_record` (JSON of every hold
action). Rollback is the pre-upgrade backup.
