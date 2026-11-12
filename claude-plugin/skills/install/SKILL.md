---
name: install
description: Install a new Oberth server on the Kubernetes cluster the user's KUBECONFIG names, without any secret entering this session. Use when the user runs /oberth:install, asks to install or set up Oberth, or the session-start note says no Oberth server is configured and the user wants one.
argument-hint: "[kubeconfig context]"
allowed-tools: Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/fetch-oberth" *), Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" *)
---

# Install Oberth

Leave `url` and `ca_pin` empty before a server exists; continue with
`/oberth:install`, which fills the installed connection settings. Some Claude
Code versions still print "2 userConfig options not yet set" for optional
empty values. That notice does not block installation; do not invent a URL or
CA pin or run configure just to silence it.

Oberth installs with its own CLI onto a cluster the user already has. This
guide drives `oberth install` non-interactively. The install creates
credentials that are shown once (the OpenBao root token and unseal key, the
first admin's client certificate (public)). They go to a secrets file, never into this
session.

Arguments given to the command: `$ARGUMENTS`

## Steps

1. **Prerequisites.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" prerequisites`.
   Missing Helm or kubectl stops the flow with installation links. Never install
   either tool yourself.
2. **The CLI.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/fetch-oberth"` (or pass an
   explicit release version). It uses the plugin's pinned release key, OpenSSL
   and SHA-256 to verify before installing into `~/.local/bin`. It needs no
   sudo, cosign, jq or Python. A verification failure stops the flow.
3. **The cluster.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" plan` with the
   chosen `--context` and answers file when available. Show its kubeconfig
   files, API URL, CA pin, node facts and remedies. Any chosen context is a valid
   target; never add a local/remote gate. With multiple contexts, ask the user.
   Never pick one yourself.
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
      NodePorts are forwarded to other ports, the next step proposes `--client-ssh-port` and
      `--client-https-port`; the user does not need to discover these numbers.
   4. Turn on assisted continuation (`--assist`), so a failed step of a run
      the agent pushed can be held for it to fix in place? Off by default.
5. **Check client reachability before install.** Save the public JSON from
   `helper plan` (the `scripts/install plan` command above) to a new local
   plan file, rerunning it with the chosen address/options if needed. Run:

   ```sh
   sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" ports --plan-file '<public plan JSON>' --client-host '<chosen DNS name>'
   ```

   Pass explicit client port overrides and `--service-type` too when selected.
   The helper uses this plan's node identities to read the matching local
   Docker cluster's published ports, including k3d serverlb/node bindings.
   Docker is optional. Propose the returned `install_flags` in the install
   review, including forwarded ports automatically; never guess a global
   `oberth` SSH alias. Explicit ports win. A remote Docker daemon's ports
   and another cluster's bindings are not this host's mappings.
   If `needs_action` is true, explain: "NodePorts are not reachable from this
   host; map them or use port-forward." Offer that network choice, then repeat
   the probe with the resulting client ports. The probe is advisory: a fresh
   server may not listen yet, and TCP or a mapping proves neither readiness
   nor TLS trust. Every cluster type uses the same install Review; this is
   not a local/remote or cluster-type gate.
6. **Local identity.** Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/trust" keygen`.
   The private client key stays under `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/` with mode 0600.
   The current identity is `${CLAUDE_CONFIG_DIR:-~/.claude}/oberth/current/`.
   In executable examples use `$HOME` for the fallback, as below.
   Reuse the user's SSH public key or generate an Ed25519 SSH key locally;
   never upload either private key. Set `--admin-ssh-public-key` to its public
   file and `--admin-csr "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/oberth/current/client.csr"`. For an SSH target,
   copy only the CSR and SSH public key and return the issued certificate over
   that authenticated SSH connection. Never set `--admin-bearer` by default.

7. **Review.** Show the complete command and wait for the user to say
   "install". Nothing runs before that.

   ```sh
   sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" run --yes --json --context '<context>' --install-secretstore \
     --secrets-file "$HOME/oberth-install-secrets.txt" \
     --admin-ssh-public-key '<local SSH public key file>' \
     --admin-csr "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/oberth/current/client.csr" --client-access none \
     --forge github --organization '<org>' --client-host localhost
   ```

   Save the public install JSON result outside the repository: add-repo uses
   its exact `server_url`, `git_url` and `ca_sha256` to bind the selected server.
   Include the probed client port flags in this reviewed command.

   The secrets file must be a new file outside any git worktree. The install
   refuses anything else.
8. **Run it** exactly as reviewed. It takes several minutes: run it in the
   background or with the longest command timeout, and never interrupt it.
   The output names the secrets file and its SHA-256, and nothing secret.
   Tell the user to move that file off this machine (a password manager, an
   offline copy) and to delete it here.

   When the install stops part-way, the secrets file it names is kept and
   may hold the only copy of the OpenBao root token and unseal key. Leave it
   where it is, tell the user, and give a rerun a new `--secrets-file` name.
9. **Finish what the output asks for:**
   - When a deploy key is printed, the user adds it at the forge as a key with
     write access to the organization's repositories. Then run the `upstream
     add` command the output printed, exactly as printed: it names the
     cluster that was installed on.
   - When the output says the client files were kept because they differ,
     this machine is set up for another Oberth server. Ask the user before
     rerunning with `--replace-client-files`; never add it yourself.
   - Use the install result's URL and CA pin with `/oberth:connect` (the
     pin came from the authenticated installer). Run the plugin's `trust
     credentials` command with the issued certificate and the local key, then
     `trust configure '<https URL>/mcp'`. It adds a user MCP entry only after
     configuration and has no Authorization header in certificate mode.
     Existing settings and env values are kept. Restart Claude Code once,
     confirm `/status` shows the mTLS client certificate row, and call
     `system_status` to prove authentication. Never read the secrets file.
   - The first admin is the user's locally generated identity. No bearer
     token is generated unless the user explicitly selects `--admin-bearer`.

## Rules

- Never print, read, grep, copy or upload the secrets file, and never pass a
  token or a key on a command line. Moving the token into a secret store goes
  through the command the install printed, which reads the file without
  showing it.
- Never delete, move, truncate or overwrite a secrets file, from this run or
  an earlier one. The install refuses a path that exists; choose a new name
  instead of removing the old file.
- A `--dry-run` prints a plan and installs nothing. After the approved
  install, run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/install" verify --context
  '<context>'` and explain every failing check and remedy.
- Never disable TLS verification, and never trust the server's CA any other
  way than `/oberth:connect` with the pin.
