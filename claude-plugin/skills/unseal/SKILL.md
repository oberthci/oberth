---
name: unseal
description: Restore a sealed OpenBao after reboot using the installer's private recovery file, without reading or printing its contents.
argument-hint: "[recovery file path]"
allowed-tools: Bash(sh "${CLAUDE_PLUGIN_ROOT}/scripts/unseal" *)
---

Run `sh "${CLAUDE_PLUGIN_ROOT}/scripts/unseal" --status --json --context '<chosen context>'`.
If sealed, ask only for the recovery file path and, for age encryption, the
identity file path. Never ask for, read, print, grep or paste the key itself.
Run the same script with `--secrets-file '<path>'`, optional
`--age-identity '<path>'`, the chosen context and `--json`. Quote paths as single
shell arguments; never interpolate unvalidated arguments as shell syntax.
Only initialized/sealed status is returned. If it fails, report the remedy and
leave the recovery file untouched. Do not enable host-bound auto-unseal unless
the user explicitly asks; its risk and TPM2 requirements are in the CLI help
and deploy/systemd/README.md. No key belongs in the transcript or on argv.

After recovery, check MCP `system_status` when reachable. Do not inspect
Kubernetes Secrets, credential files or pod environments. Never offer TPM2
auto-unseal on multi-node or managed clusters.

## Installer failure guardrail

When the installer fails or gives no clear reason, stay on the installer path:
read the public result JSON and its checks, endpoint, observed error and remedies.
Run only the documented installer verify or resume command as printed, preserving
the reviewed context and answers. If the result has no actionable reason, stop
and ask the user; never invent a workaround outside the documented commands.
Never run administrative commands inside the server Pod (including upstream add
or enrollment), directly or through another helper, to work around an installer
failure. Do not switch to another skill to bypass this rule.
Never read, pipe or export the secret store's root token or any value from the
secrets file. Give only file paths to the documented installer or unseal command;
the executable reads credentials privately. Never ask the user to paste a token
or export BAO_TOKEN for the agent.
Report only verified facts. An installer exit code or an incomplete result does
not establish whether anything was pushed to the forge. Say what was observed,
what remains deployed and what is unknown; never claim that nothing was pushed
without checking evidence that establishes that fact.
