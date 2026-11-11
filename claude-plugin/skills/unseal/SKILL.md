---
name: unseal
description: Restore a sealed OpenBao after reboot using the installer's private recovery file, without reading or printing its contents.
argument-hint: "[recovery file path]"
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/unseal *)
---

Run `"${CLAUDE_PLUGIN_ROOT}/scripts/unseal" --status --json --context '<chosen context>'`.
If sealed, ask only for the recovery file path and, for age encryption, the
identity file path. Never ask for, read, print, grep or paste the key itself.
Run the same script with `--secrets-file '<path>'`, optional
`--age-identity '<path>'`, the chosen context and `--json`. Quote paths as single
shell arguments; never interpolate unvalidated arguments as shell syntax.
Only initialized/sealed status is returned. If it fails, report the remedy and
leave the recovery file untouched. Do not enable host-bound auto-unseal unless
the user explicitly asks; its risk and TPM2 requirements are in the CLI help
and deploy/systemd/README.md. No key belongs in the transcript or on argv.
