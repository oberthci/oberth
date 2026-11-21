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

The optional [agent plugins](install.md#agent-plugins) add a run box and pin/completion tools.

## Push, observe and respond

Review the commit and obtain consent before pushing a new branch:

```sh
git push oberth HEAD:refs/heads/change -o oberth.run=1
```

An agent push without `-o oberth.run=1` is parked: no run starts. Never wait for a run that was not
created. The server's initialization instructions are the run loop; follow them exactly.

Without tools, follow `oberth log <run>` in a background shell, then use `oberth run wait <run>` and
read the state. Exit zero means the wait completed, not that CI passed.

## Show the evidence

When builds come up, paste the bridge's rendered display block verbatim in a code block. Without
tools, paste the output of `oberth run list` or `oberth run get <run>` whole. Show complete reasons
and keep full Kubernetes detail separate. Show both the accepted pushed object and the exact tested
commit when they differ. An assisted run is never promotion evidence; obtain an unassisted pass
before promoting.

## Promote and release

After consent, run `oberth promote <tested-commit-sha>`. Show the tested run, resulting
default-branch commit and that commit's CI result. If promotion is refused, follow its remedy; do
not push the default branch as a workaround. Follow [agent promotion](releasing.md#agent-promotion)
for the administrator's policy change and [project release steps](releasing.md#release-your-project)
for secret grants, saving local work, checkout synchronization and passing CI on the resulting
default commit. After release consent, run `oberth release <tag>` and show its commit, run result
and publication. A lost mutation reply means read the object's state before repeating the action.
