---
name: oberth
description: Fetch, install and join Oberth, then connect its versioned server CI skill.
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
| 2. Apply after consent | `oberth install --context <context> --yes` | Phases, profile, CA pin, SSH fingerprint, deploy key and next action. Ask them to add the deploy key with write access and copy backup.agekey off this machine. Show both printed Web view lines verbatim, including the sign-in command. |
| 3. Bind this checkout | `oberth repo add <forge-url>` | Use the forge's own SSH URL as the person gives it, with its user and path, such as `ssh://<user>@<forge>/<path>/<repository>.git`; `git@<forge>:<path>` is read from the root (`git@<forge>:~/<path>` for its home); in `<user>@<forge>:<path>` another user's relative path is in that user's home. Then the credential-free upstream URL, registered repository, remote and push line. Exit 3: show the deploy-key remedy and wait. An empty forge repository: ask the person to push its first commit with their own forge access, then retry; Oberth's deploy key is only in the cluster. |
| 4. Invite the agent identity | `oberth invite <upstream>` | Scope, expiry and invitation path, never its contents. |
| 5. Join | `oberth join <invitation-file>` | Identity, scope, profile, repository binding, push line and printed absolute stdio connection line. |

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
In Codex the bridge itself offers the run footer: tell the person to turn on Custom status line in `/experimental`, restart, and pick Oberth runs in `/statusline`.

After connecting, use server/discover and skills/list, then approve the originating server's complete oberth-ci manifest.
Load that versioned server skill and its references before CI work; it owns observation, retry-mode fix/resume, display and promotion.
A resources/read call alone does not activate it. On an upgrade, reconnect and explicitly reload the new skill, or start a fresh conversation.
Unknown old URIs or hash, size or frontmatter mismatches require rediscovery and approval before continuing.
The host must meet the complete floor in the agent guide; unsupported hosts must upgrade or use public CLI commands.
Keep origin and manifest identity attached to the loaded skill.
For a sealed store use `oberth unseal`; the selected profile supplies the recovery file. Show only status and remedy.
Never run commands inside the server Pod or read its storage or environment.
