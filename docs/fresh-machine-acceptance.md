# Fresh-machine release-candidate acceptance

Run this manual procedure on each release candidate before tagging. It tests
the first-time user path on a fresh macOS or Linux machine with a working local
`~/.kube/config` pointing at a suitable Kubernetes cluster. A macOS guest has
no nested virtualization: use a cluster reachable through that local kubeconfig,
either remote or running on the host. The client and cluster may be separate
machines; neither a virtual machine nor a particular cluster distribution is
required. The Kubernetes API address in kubeconfig must be covered by the API
server certificate's DNS or IP SANs. Use that reachable address for the installer
client-host answer; the Oberth server certificate must cover it as well.

The [bootstrap contract cell](bootstrap-cell.md) is the automated part in CI.
Record its result separately from this manual result. A green contract cell
does not establish that the steps below succeeded. A skipped or blocked required
check leaves fresh-machine acceptance incomplete for that candidate.

## Setup and rules

- Use a fresh client machine without prior Oberth installation or
  connection state, Claude Code installed and logged in, and an authorized test
  cluster without an existing Oberth installation. The cluster must support
  persistent storage and production-mode OpenBao for recovery testing. Use a
  throwaway upstream repository with permission to push. Record the candidate
  source commit, CLI/chart version and plugin commit or version to be tested.
- Test the exact candidate. Record how its plugin revision, CLI, chart and
  server image were supplied and selected, including source revisions and
  artifact digests for staged pre-tag candidates. When using published release
  artifacts, choose an explicit CLI version through the
  [verified bootstrap path](plugin-bootstrap.md). Record any operator staging
  and differences from the normal fetch path. If the candidate cannot be
  supplied, record a blocker; results from an unspecified latest release or a
  different candidate do not accept this one.
- Work in Claude Code on the fresh machine. A terminal or an operator-managed
  remote terminal session is sufficient. Wait and read the response after every
  step. Answer questions and permission prompts as a reasonable new user would,
  and record each prompt and answer with secrets masked.
- **Oberth installs no dependencies.** The operator supplies kubectl, Helm,
  host tooling and a cluster. If Claude reports a missing prerequisite, install
  it yourself from a separate plain shell, outside the guided Oberth session.
  Record this as a user step. If Oberth installs a dependency or creates a
  cluster, record a product finding. Docker, Homebrew and colima are optional
  environment choices, not universal prerequisites.
- **Never write the user's kubeconfig.** Prepare the working local
  `~/.kube/config` as an operator step before the baseline copy. Select contexts
  explicitly when running commands; do not let Oberth change the file or switch
  its current context. Keep all work within the chosen test environment; do not
  alter an unrelated host's kubeconfig, container runtime or files.
- **Keep secrets private.** Use an owner-only capture directory outside the
  checkout (`umask 077`, directory mode 0700, files mode 0600). Keep transcripts,
  stdout/stderr, MCP responses, kubeconfig baseline and any collected server or
  Pod logs there. Save recovery material through `--secrets-file` outside the
  repository; record only its path. Never paste tokens, private keys, passwords,
  recovery-file contents or raw kubeconfig into chat or reports. Do not capture
  commands that print those files, and do not enable shell tracing.
- **Time box.** Spend at most 15 minutes stuck on one step. Record a BLOCKER and
  try the obvious workaround once. If it works, continue and record it. If an
  environment prerequisite fails, report an environment result separately from
  an Oberth finding. Scan any captures even after a failed or interrupted run.
- Restart only the chosen test cluster or its host with the operator's approved
  method. Coordinate with its owner if it is remote or shared; record a blocker
  if restart cannot be performed. Recovery is a required acceptance check.

## Scenario

1. **Baseline.** Record OS/version, architecture and CPU, `claude --version`,
   and whether kubectl, Helm, Git, OpenSSL and a SHA-256 tool are present. On
   macOS, `sw_vers` and `sysctl -n machdep.cpu.brand_string` are useful; on Linux,
   use `/etc/os-release`, `uname -m` and `lscpu`. Note any optional brew, Docker
   or colima installation, and whether this is a VM. Record the candidate
   identities and start a private record of questions, commands and timing.

2. **Cluster and prerequisites (user steps).** Supply the host dependencies and
   a working local `~/.kube/config` for the chosen cluster, outside Claude's
   Oberth session. For a macOS guest, use a remote cluster or one on the host;
   do not start a nested Kubernetes virtual machine inside the guest. Prepare
   the kubeconfig with an API address reachable from the guest and present in
   the API server certificate's SANs. Use that same reachable address when the
   installer asks for the client host, and ensure the Oberth server certificate
   covers it. Record an environment blocker if cluster access cannot be supplied.
   Record `kubectl config current-context`, select that context explicitly,
   confirm `kubectl --context '<context>' get nodes` reports Ready, and record
   the Kubernetes version. Save an exact private copy of
   `~/.kube/config` in the capture directory as `kubeconfig.before` after all
   user provisioning. Ensure `KUBECONFIG` is unset or names only this file, so
   the install and comparisons use the same configuration. Do not change
   kubeconfig during the remaining steps.

3. **Plugin.** In Claude Code run `/plugin marketplace add oberthci/oberth`,
   then `/plugin install oberth@oberthci`. Select the intended candidate plugin
   revision as established in setup and verify its identity. Restart Claude if
   told to. Record any URL or token/CA-pin options requested, leave connection
   settings empty before the server exists, and record the session-start note.
   See [plugin installation](plugin-install.md) for optional settings.

4. **Install.** Run `/oberth:install` on the selected context, explicitly asking
   for the candidate CLI version. Follow the reviewed installation through to
   completion. Choose production-mode OpenBao (`--install-secretstore`, not
   in-memory dev mode), a new owner-only `--secrets-file`, certificate-based
   admin access, and assisted continuation (`--assist`) for step 7. Supply the
   test upstream and its trusted host-key pin when requested. Preserve the
   public install result for repository binding. Check the installed CLI,
   server and chart versions against the candidate. Run install verification
   on the same context and record every check and warning; required checks
   must pass. Record every question and answer, every command shown before
   execution, elapsed time and every warning/error verbatim with secrets
   masked. Keep recovery material and private client keys out of the session.

5. **Connect.** Restart Claude if instructed. Follow `/oberth:connect` if needed,
   with the selected server's CA pin and local client certificate/key paths.
   Check that MCP tools are available and call `system_status`. Record the
   tool count, server version, authentication as mTLS and public status summary.
   Do not bypass CA/hostname verification or substitute bearer authentication
   to turn a failed certificate connection into a pass.

6. **Smoke run.** Follow phases 1–2 of the
   [example repository walk](#example-repository-walk) to register and bind the
   throwaway repository using `/oberth:add-repo`. Copy the v0.22.7 variant
   with its test, sidecar and parameter handoff; each authored container requests
   `cpu: 100m`. Use an explicit image and enough authored memory,
   storage and limits for the chosen cluster; save the authored YAML. Commit
   and push through the server-bound Oberth remote. Record the pushed SHA,
   run ID, whether the run passed and how Claude reported it. Inspect the step
   Pod with `kubectl --context '<context>' -n '<namespace>' get pod '<pod>' -o yaml`;
   keep the capture private. Compare the authored containers' requests and
   the authored placement (including selectors/affinity) and priority with the
   submitted YAML; require Oberth to preserve those fields unchanged.

   Separately inventory generated helpers: Argo's `wait` and `init`,
   `oberth-executor-token`, and `oberth-prepare-nonroot` when that profile is
   selected. Compare Argo-generated helpers with the controller's native
   `executor.resources`; record the stored operator value or installer defaults.
   Compare Oberth-owned helpers' requests and limits, including the token waiter's
   conditional ephemeral-storage budget, with the
   [documented helper resource contract](native-scheduling.md#what-oberth-writes-into-the-users-workflow-and-pods).
   Report their CPU, memory and storage requests and their scheduling cost in
   the Pod's effective requests, accounting for Kubernetes' init/steady-state
   aggregation. The documented helper budgets are expected and must be
   reported. Flag unexpected generated helpers or changes to their documented
   budgets, and any Oberth mutation of authored requests, placement or priority.
   Attribute cluster admission defaults separately, recording each field,
   value and source. A passing run alone does not establish these checks.

7. **Held-run round trip.** In the same credential-free branch build, add a
   committed file such as `expect.txt` containing `broken` and a step whose
   command fails unless it contains `fixed`. The step must be eligible for
   [assisted continuation](assisted-continuation.md): no authored retry strategy
   and no secret-store paths. Commit and record the exact SHA. Obtain an events
   cursor with `events_since`, then open `events_wait` before pushing, scoped
   to this repository/branch with `kinds: [run.held, run.finished]` and that
   `after_id`. Push with `git push -o oberth.assist=1` to the bound Oberth remote.
   Keep waiting with `next_after_id` after a timeout or page; a retention gap
   requires reconciliation through `events_since`/`run_get`. See
   [agent events](agent-events.md).

   Require a `run.held` event for this push. Read `run_get` to verify the
   exact pushed SHA and run ID, then `run_hold_list` to verify the held step
   and `patch_capable: true` before editing. Through `run_hold_patch`, apply a
   unified diff changing only `expect.txt` from `broken` to `fixed`, then call
   `run_continue` for that run and step before its hold deadline. Wait for
   completion and read `run_get` for `status: passed`, the same SHA and run ID,
   and a `display_reason` containing `not evidence`. Read `assisted: true` from
   `run_hold_list` or the full `run_get` view (`summary: false`); the default
   `run_get` summary does not include it. Record the event ID, run ID,
   SHA, step, patch digest and result. The assisted pass must not count as
   green evidence for its SHA, become a promotion source or publish upstream.
   A premature `run.finished`, missing hold or ordinary green classification
   is a finding. Keep the original smoke pass separate from this assisted run.

8. **Restart and unseal.** Record the successful smoke and assisted run IDs and
   SHAs, verify OpenBao is initialized and unsealed, and preserve recovery-file
   and optional age-identity paths privately. Restart the actual test cluster
   or the host running it using the operator's normal method. Restarting only
   the client of a remote cluster does not exercise server recovery. Record
   evidence that the cluster/host restarted (for example changed boot identity
   or recreated server Pods) and reconnect from the fresh client. Restore any
   operator-owned forwards needed for the same endpoint.

   Check initialized/sealed status without printing recovery material:

   ```sh
   oberth unseal --status --context '<context>'
   oberth unseal --secrets-file '<recovery-file>' --context '<context>'
   oberth unseal --status --context '<context>'
   oberth install --verify --json --context '<context>'
   ```

   Use the chosen OpenBao namespace if different from the default, and
   `--age-identity '<identity-file>'` for encrypted recovery. Require initialized
   and sealed before unseal, then initialized and unsealed after it. An
   auto-unsealed environment does not prove the manual unseal check; record it
   and repeat with a suitable test setup. Require install verification checks
   to pass, reconnect over mTLS and call `system_status`. Read both retained
   runs: IDs, SHAs and results must survive, including the assisted flag and
   `not evidence` reason. Record recovery duration and any manual workarounds.

9. **Kubeconfig integrity.** Compare `kubeconfig.before` with
   `~/.kube/config` using `cmp -s` for a result without displaying credentials.
   Any change is a finding. Inspect a diff only in private and include only a
   sanitized diff in the report. Oberth must never write the user's kubeconfig.

10. **Re-run.** Run `/oberth:install` again on the same context and candidate
    version. It should detect the existing installation and leave it working.
    Record what it asks and does, verify installation and mTLS `system_status`
    again, and repeat the kubeconfig comparison after this step.

11. **Clean up.** Record what remains: the test installation, repository and
    branches, local CLI/plugin/settings, certificate and SSH files, recovery
    file, captures and user-installed dependencies. Remove test resources or
    stop the test cluster according to the operator's agreed lifecycle; for
    the colima example, stop colima and leave the client VM running. Do not stop
    a shared remote cluster or remove unrelated resources. Record leftovers
    and any retained private evidence by path only. Keep captures available
    privately until the final scan, then dispose of them and test credentials
    under the operator's normal retention policy.

12. **Secrets scan and report.** Before sharing anything, scan all captured
    stdout/stderr (including failures), terminal transcripts, MCP responses,
    and collected server/Pod logs, plus the proposed report. Use an offline
    scanner reading credential files privately, never secrets in argv, chat,
    or shell history. Check the actual recovery values, tokens and private
    keys created during the test in raw, base64 and decoded forms, including
    PEM body lines and values split across output chunks or JSON events.
    Also check for unexpected credential material beyond that inventory.
    Report only the scan result and affected stage/channel names, never a
    matched value or matching line. Treat any exposure as a finding, withhold
    raw captures, rotate affected credentials as appropriate, and rescan the
    sanitized report. An incomplete inventory or interrupted/failed scan is
    not a clean result. Reports contain public summaries only; raw evidence
    and secrets files must never be uploaded with release evidence.

## Example repository walk

Use [`examples/example-repo/v0.22.7/`](../examples/example-repo/v0.22.7/.oberth/build.yaml)
as the repository for scenario steps 6 and 7. Its
[README](../examples/example-repo/README.md) describes both variants and the
recorded validation boundary. `next/` is **DRAFT**: it decodes at the recorded
`int/v0.23.0` snapshot, but source inputs, authored security contexts and existing
cache claims are still refused there. It is not a replacement acceptance path
until the assembled v0.23.0 candidate admits and runs it. A local admission/Build
test is not a live installation, cancellation, promotion or release result.

Keep the scenario's candidate pinning, private capture, time box, explicit
context, recovery and final secrets scan rules. Execute this walk only in the
throwaway repository chosen in setup. The commands below describe a manual
acceptance run; do not push an Oberth development checkout. Replace angle-bracket
values with the actual run, repository and environment identities.

1. **Copy and bind (scenario step 6).** Clone the empty throwaway upstream,
   then copy the contents of the selected variant, including `.oberth`, into
   its root. Supply the actual candidate checkout path:

   ```sh
   git clone '<throwaway-upstream-url>' example-repo
   cd example-repo
   cp -R '<candidate-checkout>/examples/example-repo/v0.22.7/.' .
   oberth validate --runner-image-prefixes= .
   ```

   Expected: local validation succeeds. The explicit empty prefix list matches
   an installation without an image allowlist; when the operator uses an
   allowlist, validate with that same list and allow the authored digest pins.
   Local validation checks structure, not live grants or identities. Register
   and bind this checkout through `/oberth:add-repo` using the public install
   result. Expected: the canonical repository key and the server-bound
   `oberth` remote, with no change to kubeconfig. Use the selected upstream's
   default branch name (the commands below assume `main`).

   ```sh
   git add .
   git commit -m 'Add example pipeline'
   git push oberth HEAD:refs/heads/main
   ```

2. **Read the green build.** Follow this push's run with `events_wait` or
   `run_wait`, then `run_get` and `run_logs`. Record the exact SHA and run ID.
   Require `status: passed` and these application lines in task order:

   ```text
   test: app passed
   test: source passed
   test: sidecar passed
   build: <64-hex SHA-256 of app.sh>
   handoff: <the same digest> (branch)
   cleanup: complete
   ```

   `wait` exits silently when `cancel.txt` contains `no`. The HTTP sidecar is
   started and stopped by Argo; no Service or operator port-forward is needed.
   Compare the producer's output parameter `digest` with the report task's
   input and log; this is an Argo parameter handoff between separate Pods.
   Finish the authored-resource and generated-helper Pod comparisons in
   scenario step 6. v0.22.7 supplies security contexts and private `/tmp` mounts;
   compare requests, limits and placement without mistaking these documented
   changes for authored resource changes.

3. **Held build (scenario step 7).** On a separate `demo/assist` branch from
   the green commit, change only `expect.txt` to `broken`. Keep `cancel.txt`
   at `no`. Use scenario step 7's cursor, subscription, assist push and
   patch/continue procedure to change `expect.txt` to `fixed` in the held
   `test` step. Expected before the patch: `test: app passed`, followed by a
   hold for `test`. Expected after continuation: the remaining success lines,
   the same run ID/SHA, `assisted: true`, and `not evidence`. No upstream
   publication or promotion evidence may result. The build declares no
   secrets or retry strategy. Keep this branch separate from the green source
   used below; changing the local file alone does not make its SHA green.

4. **Stop, then Terminate.** Start `demo/cancel` from the original ordinary
   green commit. Set `cancel.txt` to `yes`, commit and push this branch through
   the bound remote. Wait for `cancel: waiting for Stop` in the `wait` step.

   ```sh
   oberth run cancel '<cancel-run-id>' --reason 'acceptance Stop' --json
   ```

   Expected: cancellation acknowledgement with a warning that repeating an
   active cancellation uses Argo Terminate. With the private Workflow identity
   from the run, observe `spec.shutdown: Stop` and
   `cleanup: waiting for Terminate` in the exit handler. Keep this run active
   for the second call; each wait is bounded to 600 seconds.

   ```sh
   oberth run cancel '<cancel-run-id>' --reason 'acceptance Terminate' --json
   kubectl --context '<context>' -n '<pipeline-namespace>' get workflow '<workflow-name>' -o jsonpath='{.spec.shutdown}'
   ```

   Expected: `Terminate`; the active exit handler is stopped and does not print
   `cleanup: complete`. Retain both shutdown observations and Argo's terminal
   phase/reason, plus the cancelled run record. Oberth does not delete the
   Workflow; Argo TTL eventually removes it. If the run finishes before the
   second call, repeat on a new cancel commit and record the missed window.
   Release-tier and promotion cancels start with Terminate, so use this ordinary
   branch run to prove escalation. Do not promote the cancel commit.

5. **Promote an ordinary green change.** Start `demo/change` from the original
   green commit, with `expect.txt=fixed` and `cancel.txt=no`. Add `change.txt`,
   commit, and push through `oberth`. Expected: a new ordinary green build with
   the same application output. Promote its exact tested commit:

   ```sh
   printf '[{"sha":"%s","ref":"demo/change"}]\n' '<green-change-sha>' > '<private-capture>/promotion-sources.json'
   oberth promotion create --repo '<repo-key>' --branch main --sources-file '<private-capture>/promotion-sources.json' --json
   ```

   Use an admin connection or a promotion grant for this source/target tuple.
   The one-source array forces promotion CI even for a fast-forward. Follow
   `promote_status` and the associated run. Expected: tests and handoff succeed
   with `(promotion)` in the handoff line, publication completes and the upstream
   `main` reaches the reported result SHA. Assisted or cancelled runs are never
   the source. Read the actual promotion result; an accepted request alone is
   not delivery.

6. **Grant one secret and release on a tag.** Ask the authorized OpenBao
   operator to create KV v2 path `oberth/data/release/example-repo` with one
   nonempty `marker` field, using a private acceptance-only value recorded in
   the scan inventory. Do not print it. Grant the exact tuple through the
   authenticated admin connection:

   ```sh
   oberth mcp call access_allow '{"repo":"<repo-key>","workflow":".oberth/release.yaml","step":"read-secret","secret":"oberth/data/release/example-repo"}'
   ```

   Synchronize OpenBao roles/policies with `oberth secretstore sync` and install
   the scoped identities through the reviewed secret-store upgrade on the same
   explicit context. Confirm reconciliation is healthy before tagging. A grant
   record alone does not prove its ServiceAccount and store policy exist.
   Fetch the delivered default-branch result and create a new annotated version
   tag on that exact commit, reachable from the fresh upstream default branch:

   ```sh
   git fetch origin main
   git tag -a v0.1.0 '<promotion-result-sha>' -m 'Example release'
   git push oberth refs/tags/v0.1.0
   ```

   Expected: release admission selects `.oberth/release.yaml`; its run has the
   tag's peeled SHA and release authority. The test and digest lines repeat,
   with `(tag)` in the handoff, then:

   ```text
   secret: marker present
   release: refs/tags/v0.1.0
   ```

   The final line uses the server's release-tag parameter (the full ref on
   v0.22.7). Require a passed release run, no marker value in any capture,
   no secret-store settings in the test/report steps or HTTP sidecar, and the
   file/step-scoped identity only on `read-secret`. This demonstration reads a
   secret and completes a tag release; it has no external application publisher.
   Return to scenario step 8 for restart/unseal and the remaining required checks.

### Three deliberate refusals

Perform each case separately, restore the original state, and require the
ordinary green walk to work again. Save only sanitized refusals. The repository's
`TestExampleRepoRefusals` loads these same files through admission and Build.

| Case | Deliberate change | Expected refusal and recovery |
| --- | --- | --- |
| Forge a run fact | In a disposable copy of `build.yaml`, add `spec.arguments.parameters: [{name: oberth-sha, value: forged}]`; run `oberth validate --runner-image-prefixes= <copy-root>` | Nonzero result containing `server-owned`. Remove the declaration; consume the workflow parameter without declaring it. |
| Read the node filesystem | In a separate copy, add `spec.volumes: [{name: host, hostPath: {path: /}}]`; run the same validation | Nonzero result containing `mounts the node filesystem`. Remove the volume; authored node paths are never a cache grant. |
| Use another step's secret grant | On the test installation, revoke the exact `read-secret` tuple with `access_revoke` and grant the same file/path to `report` with `access_allow`. Attempt a fresh annotated version tag such as `v0.1.1-refused` on the delivered commit | Release admission refuses `secret grant denied` for `read-secret`; no granted secret Pod starts. Revoke `report`, re-grant `read-secret`, reconcile identities/policies and retry with a fresh version tag. A grant to a different workflow file also supplies no authority. |

Use only the acceptance-only secret for the last case. Keep the refused tag's
local identity in the private record and do not move or reuse an admitted tag.
The CLI's local validator cannot establish the live grant refusal; observe the
server's release admission. A push refusal or recorded failed admission must
name the grant boundary, rather than being a connectivity or tag-ancestry error.

## Report template

Write the report in one fenced Markdown block using this structure. PASS
requires all steps, principle checks, restart/unseal and the final scan to
complete for the identified candidate. PASS WITH ISSUES records nonblocking
findings with all required checks completed; FAIL names the blocked or failed
step and distinguishes environment from product causes. Record skipped checks
explicitly and do not claim acceptance for them. Report documented helper
budgets and their scheduling cost as expected behavior. Scheduling findings are
Oberth mutations of authored requests, placement or priority, or unexpected
generated helpers or helper-budget changes. Attribute cluster defaults
separately.

```markdown
# Oberth fresh-machine acceptance — <date time and timezone>
Result: PASS | PASS WITH ISSUES | FAIL at step N (environment/product)
Candidate source SHA: <sha>   Artifact source/selection: <public identity>
Oberth CLI/server/chart: <versions>   Plugin: <version/commit>   Claude Code: <version>
OS: <version>   CPU/architecture: <detail>   Cluster: <distribution/version, local/remote>
Bootstrap contract CI result: <result and public run identity>
Total time from step 3 to step 6 green: <min>   Total scenario time: <min>

## Steps
| # | Step | Result (ok/issue/blocker/skipped) | Time | Note (one line) |
|---|------|-----------------------------------|------|-----------------|

## Findings (most severe first)
### F1 [BLOCKER|BUG|FRICTION|DOCS] <one-line title>
- Step: <n>
- Cause: <environment/product>
- Expected: <...>
- Actual: <...> (verbatim error, secrets masked)
- Workaround used: <...|none>

## User steps (dependencies I installed myself; not Oberth bugs)
- <tool> — why Claude said it was needed — install command used (secrets masked)

## Questions Oberth asked and my answers
1. <question> -> <answer, secrets masked>

## Run checks
- Smoke run: <id, exact pushed SHA, result>
- Held run: <event ID, run ID, exact pushed SHA, step, patch digest>
- Continuation: <status, assisted flag, not-evidence reason, no upstream publication>
- Parameter handoff and sidecar: <producer/consumer digest, sidecar result>
- Cancel: <run ID, observed Stop then Terminate, exit handler and retained Workflow>
- Promotion: <source SHA, promotion ID, result SHA and upstream publication>
- Tag release: <annotated tag object/peeled SHA, run ID, scoped grant, marker-presence result>
- Three deliberate refusals: <server-owned parameter, node filesystem, wrong-step secret grant; restored and rechecked>

## Restart and recovery
- Restart method and evidence: <public summary>
- OpenBao before unseal: <initialized/sealed>; after: <initialized/unsealed>
- Recovery-file path: <path only>; age-identity path if used: <path only>
- Install verification, mTLS status and recovery time: <summary>
- Retained smoke/assisted run identities and classification: <summary>

## Kubeconfig comparison
<"identical" before and after re-run, or sanitized diff; never raw kubeconfig>

## Smoke Pod scheduling
- Authored container requests, placement and priority: <authored/observed values; require no Oberth mutations>

| Generated helper | Kind (init/regular) | CPU request | Memory request | Ephemeral-storage request | Limits | Matches documented budget? |
|---|---|---|---|---|---|---|
| <wait/init/oberth-executor-token; nonroot preparation if selected> | <kind> | <value> | <value> | <value/absent, per documented condition> | <values> | <yes/no, detail> |

- Helper scheduling cost: <contribution to effective Pod requests using Kubernetes init/steady-state aggregation; include CPU, memory and storage>
- Cluster admission defaults: <fields, values and their source, attributed separately>

## Principle checks
- Oberth installed or created a dependency or cluster itself: yes/no (<detail>)
- Oberth mutated authored container requests, placement or priority: yes/no (<comparison above>)
- Unexpected generated helpers or changes to documented helper budgets: yes/no (<inventory comparison above; documented budgets are expected>)
- Oberth wrote ~/.kube/config: yes/no

## Secrets scan
- Coverage: <capture stages/channels, report, credential inventory coverage>
- Result: <clean/exposure/incomplete>; findings: <stage/channel names only>
- Sanitized report rescan: <result>

## Leftovers after cleanup
- <resources and public descriptions; private files by path only>
```
