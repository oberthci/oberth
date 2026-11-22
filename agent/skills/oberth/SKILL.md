---
name: oberth
description: Install and connect Oberth, follow CI runs, assist held steps, promote tested commits and release.
---

Use the public CLI and connected tools. Reuse consent already given for the same change.
Show the concrete target and changes before an unapproved install, commit, push, promotion or release.
Secrets, invitations and recovery material enter commands by private-file path; never read or paste their contents.

Check the trusted user installation with `oberth version`; if absent, after consent run `sh <skill-directory>/fetch-oberth`.
Run every command below using the absolute executable path printed by fetch-oberth or returned by `command -v oberth`.
Reject a checkout executable or a relative path. The fetcher needs curl or wget, openssl, and sha256sum or shasum.
It carries its release public key; no checkout or machine configuration is needed.
Use `sh <skill-directory>/fetch-oberth --force <version>` only for a reviewed replacement.
Do not install prerequisites, change the trust store, edit agent settings, or change global Git or SSH configuration.

First run, in order (reuse an existing installation, repository or identity). Ask the person which kube context; never pick one:

| Step | Run | Show the person |
| --- | --- | --- |
| 1. Review the chosen context | `oberth install --context <context> --dry-run` | The whole review: target, address, images, store and files written. |
| 2. Apply after consent | `oberth install --context <context> --yes` | Phases, profile, CA pin, SSH fingerprint, deploy key and next action. Ask them to add the deploy key with write access and copy backup.agekey off this machine. |
| 3. Bind this checkout | `oberth repo add <forge-url>` | Use the forge's own SSH URL as the person gives it, with its user and path, such as `ssh://<user>@<forge>/<path>/<repository>.git`; as in Git, a relative path in `<user>@<forge>:<path>` is in that user's home. Then the credential-free upstream URL, registered repository, remote and push line. Exit 3: show the deploy-key remedy and wait. An empty forge repository: ask the person to push its first commit with their own forge access, then retry; Oberth's deploy key is only in the cluster. |
| 4. Invite the agent identity | `oberth invite <upstream>` | Scope, expiry and invitation path, never its contents. |
| 5. Join | `oberth join <invitation-file>` | Identity, scope, profile, repository binding, push line and printed absolute stdio connection line. |
| 6. Push the approved commit to a new branch | `git push oberth HEAD:refs/heads/<new-branch> -o oberth.run=1` | Commit, started run and watch link. Parked means no run; do not wait for one. |
| 7. Without connected tools, follow in a background shell | `oberth log <run>` | Failed step's last lines on failure; completion line when finished. |
| 8. Without connected tools, read the final state | `oberth run wait <run>` | The complete run display block. Exit 0 also covers a failed run; read its verdict. |

An invitation scopes access to every repository in one upstream: the first component of
the full `<upstream>/<organization>/<repository>` name, not the full repository name.

Use `--url` and `--ssh-port` on install only for mapped ports; keep them identical between review and apply.
The selected profile, from install or join, pins the Oberth server's SSH keys; pass `--known-hosts` to repo add only to replace them, never with the forge's keys.
If `.oberth/` is absent, run `oberth init`, review the YAML and obtain consent for its commit before pushing.
Show the stdio line from join verbatim to the person: register exactly its absolute executable path in the printed `oberth mcp --profile <uplink-profile>` line.
The bridge uses that joined uplink or the checkout binding; it refuses administrator profiles.
Keep using the CLI until those tools are connected. Do not edit host settings yourself.
Right after join, tell the person how Claude Code's run box finds the CLI: it uses `~/.local/bin/oberth` and, in this checkout, the joined profile.
If join printed another path, or Claude Code starts outside this checkout, ask them to run `/plugin configure oberth@oberthci` and set executable to the path in the join line, profile to the uplink profile that line names, and leave runs blank for three.
The plugin gives the person `/oberth-runs` and `/oberth-watch <run>`; you use the bridge's tools, the plugin adds none.
With tools connected, follow the server's initialization instructions; they are the authority for the run loop.
Each run result from those tools ends with its display block; paste that block verbatim.
Without those tools, use steps 7-8 and paste `oberth run get <run>` whole.
During held-step assistance never print the environment or credential files.
After inspection and an authored fix, use `oberth run hold release <run> --step <step>` to release the hold.
Report only observed results; show complete reasons and keep native detail separate.

After promotion consent, run `oberth promote <commit-sha>` and show the tested run, resulting main and its CI result.
Join keeps the current administrator profile for this installation; promote and release need no profile override.
For the first release, run `oberth secret put --repo <repo> --file <pipeline-file> --step <step> <secret-path> < <value-file>`.
The shell passes the file to stdin; do not open it yourself. Show only the grant and storage receipt.
Promotion does not move the checkout. Save local work, synchronize to the reported resulting
default-branch commit, and wait for its passing CI; see [project releases](../../../docs/releasing.md#release-your-project).
After promotion and release consent, run `oberth release <tag>`.
Show the tag, commit, release result and publication event.
For a sealed store use `oberth unseal`; the selected profile supplies the recovery file. Show only status and remedy.
Never run commands inside the server Pod or read its storage or environment.
