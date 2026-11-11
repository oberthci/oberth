---
name: unseal
description: Recover a sealed production OpenBao after reboot without exposing its key. Use for /oberth:unseal or the sealed recovery hint.
argument-hint: "[secrets-file path]"
---

# Unseal Oberth

Arguments: `$ARGUMENTS` are a file path, never a key.

1. Check `command -v oberth kubectl`. If kubectl is missing, give
   https://kubernetes.io/docs/tasks/tools/ and stop; never install it.
2. List kubeconfig contexts and have the user pick the installed cluster.
   Ask for the installer secrets file's path (and the age identity file's
   path when encrypted). Never read, cat, grep, source or preview those files.
   Never ask for a key, password or token in chat, argv or environment.
3. Run `oberth unseal --context '<chosen-context>' --secrets-file '<path>'`.
   Add `--age-identity '<identity-path>'` for age encryption, and `--namespace`
   or `--pod` only for the user's custom OpenBao coordinates. Pass paths only.
   Do not trace commands. The CLI suppresses remote output on errors.
4. Expect `sealed: false`. Then check MCP `system_status`; the server may need
   a short interval to refresh its probe. If MCP is still unavailable, explain
   the status and check the chosen cluster's public pod state. Never inspect
   Kubernetes Secrets, credential files or pod environments.

Auto-unseal is off by default. Never offer it on multi-node or managed clusters.
Only when explicitly requested, explain: anyone who boots the disk on this
machine gets the secrets. Follow deploy/systemd/README.md on the actual node,
with explicit risk acceptance. TPM2 is mandatory; never substitute a disk key.
