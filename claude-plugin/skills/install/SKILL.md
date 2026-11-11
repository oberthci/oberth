---
name: install
description: Install a new Oberth server on the Kubernetes cluster the user's KUBECONFIG names, without any secret entering this session. Use when the user runs /oberth:install, asks to install or set up Oberth, or the session-start note says no Oberth server is configured and the user wants one.
argument-hint: "[kubeconfig context]"
---

# Install Oberth

Oberth installs with its own CLI onto a cluster the user already has. This
guide drives `oberth install` non-interactively. The install creates
credentials that are shown once (the OpenBao root token and unseal key, the
first uplink's bearer token). They go to a secrets file, never into this
session.

Arguments given to the command: `$ARGUMENTS`

## Steps

1. **Prerequisites.** Run `command -v kubectl helm`. If either is missing,
   stop and tell the user to install it (kubectl:
   https://kubernetes.io/docs/tasks/tools/, helm:
   https://helm.sh/docs/intro/install/; a k3s host has kubectl). Never install
   them yourself.
2. **The CLI.** When `command -v oberth` finds nothing, install it into the
   user's home. No sudo is needed, and the script verifies the checksums and
   the release signature:

   ```sh
   export OBERTH_INSTALL_DIR="$HOME/.local/bin"
   curl -fsSL https://oberth.ci/install.sh | sh
   ```

   Then use `"$HOME/.local/bin/oberth"` when that directory is not on PATH.
3. **The cluster.** List the contexts with `kubectl config get-contexts`. Any
   cluster they reach is a valid target, local or remote. With one context,
   name it and its API server. With several, ask the user which one, or take
   the context given as the argument. Never pick one yourself. The chosen
   name becomes `--context`.
4. **Ask, in plain words, four things:**
   1. Real use or a quick evaluation? Real use is `--install-secretstore`
      (OpenBao in production mode; the user keeps the unseal key). An
      evaluation is `--install-secretstore-dev` (in memory, lost on restart).
   2. Where does green code go? For GitHub, Codeberg or GitLab:
      `--forge github|codeberg|gitlab --organization <org>`. For any other
      forge: `--upstream-url ssh://git@<host>[:<port>]/<org>`, together with
      `--expected-host-fingerprint SHA256:...`, the forge's SSH host-key
      fingerprint as its admin or documentation states it. Never take a
      fingerprint from a scan of the forge yourself: that is trust on first
      use.
   3. How do clients reach the server? On this machine: `--client-host
      localhost`. From other machines: this host's name or address. When the
      NodePorts are forwarded to other ports, add `--client-ssh-port` and
      `--client-https-port`.
   4. Turn on assisted continuation (`--assist`), so a failed step of a run
      the agent pushed can be held for it to fix in place? Off by default.
5. **Review.** Show the complete command and wait for the user to say
   "install". Nothing runs before that.

   ```sh
   oberth install --context '<context>' --install-secretstore \
     --secrets-file "$HOME/oberth-install-secrets.txt" \
     --forge github --organization '<org>' --client-host localhost
   ```

   The secrets file must be a new file outside any git worktree. The install
   refuses anything else.
6. **Run it** exactly as reviewed. It takes several minutes: run it in the
   background or with the longest command timeout, and never interrupt it.
   The output names the secrets file and its SHA-256, and nothing secret.
   Tell the user to move that file off this machine (a password manager, an
   offline copy) and to delete it here.

   When the install stops part-way, the secrets file it names is kept and
   may hold the only copy of the OpenBao root token and unseal key. Leave it
   where it is, tell the user, and give a rerun a new `--secrets-file` name.
7. **Finish what the output asks for:**
   - When a deploy key is printed, the user adds it at the forge as a key with
     write access to the organization's repositories. Then run the `upstream
     add` command the output printed, exactly as printed: it names the
     cluster that was installed on.
   - When the output says the client files were kept because they differ,
     this machine is set up for another Oberth server. Ask the user before
     rerunning with `--replace-client-files`; never add it yourself.
   - The install configured this machine's client files (`~/.config/oberth`)
     and, when Claude Code is on PATH, its CA trust and this plugin. The user
     then enters the server in `/plugin configure oberth@oberthci`: the URL is
     `OBERTH_BASE_URL` from `~/.config/oberth/env` plus `/mcp`; the token is
     the `OBERTH_UPLINK_TOKEN` line of the secrets file, which the user opens
     and copies; the CA pin is what `oberth client pin` prints. One restart of
     Claude Code follows.
   - Promotion to a target branch needs a single-use grant from an admin
     uplink. The admin uplink belongs to the user, with a key you do not
     hold. Tell them the command from the install output. Never create one
     for yourself.

## Rules

- Never print, read, grep, copy or upload the secrets file, and never pass a
  token or a key on a command line. Moving the token into a secret store goes
  through the command the install printed, which reads the file without
  showing it.
- Never delete, move, truncate or overwrite a secrets file, from this run or
  an earlier one. The install refuses a path that exists; choose a new name
  instead of removing the old file.
- A `--dry-run` prints a plan and installs nothing. After the approved
  install, check the server with `oberth status` (with
  `~/.config/oberth/env` sourced).
- Never disable TLS verification, and never trust the server's CA any other
  way than `/oberth:connect` with the pin.
