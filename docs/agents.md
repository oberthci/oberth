# Connect an agent

An uplink is an authenticated agent identity, separate from the administrator. A pipeline
declaration cannot widen its access. Use the [agent skill](../agent/skills/oberth/SKILL.md) for
setup.

## Install, invite and join
The person chooses the Kubernetes context. Use the [install guide](install.md#review-and-install) to
show the review before applying; reuse consent already given for the same concrete change. Show the
deploy key and next actions, including the separate private recovery copies.

An administrator invites an upstream, granting access to every repository in it:

```sh
oberth invite <upstream>
```

The administrator registers each repository once using the
[verified forge fingerprint](install.md#review-and-install). Join binds no fresh clone; there a
joined person or agent lists the visible repositories and binds the checkout:

```sh
oberth join <invitation-file>
oberth repo list
oberth repo add <forge-url>
```

For a joined profile, `oberth repo add <forge-url>` selects a visible registered repository by its
forge URL, pins the installation's SSH host keys that join saved from the invitation, and prints
`bound: <upstream>/<organization>/<repository>`. `--upstream <name>` must match its upstream;
`--known-hosts <file>` replaces the saved pins. An unavailable repository says
`Repository is not available in this uplink's scope.` and prints the administrator's registration
command. Remedy: `Ask an administrator to register it and check this uplink's scope.`

Show an invitation's scope, expiry and path, never its contents. Unused invitations last at most
seven days. Use `--expires 1h` for less time; run `oberth invite <upstream>` for a new invitation.
Join removes the spent file. For a checkout bound to another installation, use a fresh clone or
deliberately run `oberth repo add <forge-url>` there. An administrator profile stays selected;
select the joined profile with `OBERTH_PROFILE=<uplink-profile>`:

```sh
OBERTH_PROFILE=<uplink-profile> oberth repo list
OBERTH_PROFILE=<uplink-profile> oberth repo add <forge-url>
```

The checkout uses the uplink for pushes. The agent connection names its uplink profile explicitly;
do not give it administrator credentials. If redemption consumed an invitation before a later
failure, request a fresh invitation. `oberth commit status <ref>` shows the visible CI state of a
commit. `oberth renew` renews a certificate within its permitted renewal period. After expiry or
revocation, get a new invitation.

## Connect the tools
MCP means Model Context Protocol, the request protocol used by the agent's tools. The supported
connection is the standard-input/output bridge `oberth mcp`. Join prints an absolute executable path
with the uplink profile explicitly selected:
`<absolute-oberth-path> mcp --profile <uplink-profile>`. Register exactly that line. `oberth mcp`
refuses to start with an administrator profile. The host launches that process; it needs no
private-CA trust edit or client-key paths. Do not substitute a bare executable name or a file from
the checkout. Keep using CLI commands until the tools are connected.

The bridge uses the selected CLI profile and its pinned HTTPS trust. Without a profile it gives
setup instructions and no tools; it does not try another installation.

Only MCP 2026-07-28 is supported, including enrollment and standalone CLI calls.
The complete host floor is Claude Code 2.1.331+ in the interactive terminal main conversation,
v2 runtime, direct Anthropic connection, feature flags and MCP skills enabled; Fable, Opus 4.8,
Sonnet 5, Haiku 5.5 or later. Pro/Max/Ultra qualify; Team/Enterprise need organization enablement.
One-shot, SDK and cloud sessions are outside this floor. Other hosts need equivalent skill loading,
Tasks and form interaction. Tasks support alone (2.1.304) is insufficient.
Clear CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS=0 and CLAUDE_CODE_DISABLE_MCP_TASK_BACKGROUND=1,
then start a new session and rediscover/attach. Keep the same server name, transport, command/URL,
args, env, headers and timeout for same-session recovery. Otherwise read durable state and attach
afresh. Recovery never restarts CI. Unsupported hosts must upgrade or use the standalone CLI.

## Push, observe and respond
After reviewing the commit and obtaining consent, request CI with
`git push oberth HEAD:refs/heads/change -o oberth.run=1`. Without that option a push is parked:
no run starts. Follow the server's discovery instructions for the CI loop.
Without tools, follow `oberth log <run>` in a background shell, then `oberth run wait <run>`.
Exit zero means the wait completed; read the verdict.

## Show the evidence
When builds come up, paste the bridge's rendered display block verbatim in a code block. Without
tools, paste the output of `oberth run list` or `oberth run get <run>` whole. Show complete reasons
and keep full Kubernetes detail separate. Show both the accepted pushed object and the exact tested
commit when they differ. An assisted run is never promotion evidence; obtain an unassisted pass
before promoting.

## Promote and release
After consent, use `oberth promote <tested-commit-sha>` and show the resulting default commit's CI.
On refusal, follow its remedy; do not push the default branch as a workaround.
Follow [agent promotion](releasing.md#agent-promotion) for policy and
[project releases](releasing.md#release-your-project) for grants and checkout synchronization.
After release consent, use `oberth release <tag>`; show its commit, run and publication.
Resolve lost mutation replies from durable state before repeating an action.

## Resume a failed run
After consent, commit and push F parked, then `oberth run resume <id> <commit>`
(`run_resume {id,commit}`). Argo retries failed work, retaining passed outputs and volumes.
R2 is Assisted, never evidence. On failure fix/resume again; after a pass and consent, rerun once
for a fresh unassisted pass to promote. Changed pipelines or grants require a full pushed run.
Carried caches and effects are not fresh evidence; same-Pod resume is unavailable.
