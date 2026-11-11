# Private unseal

After a production OpenBao restart:

```sh
oberth unseal --context '<chosen context>' --secrets-file "$HOME/oberth-install-secrets.txt" --json
```

For an age-encrypted recovery file, add `--age-identity '<identity file>'`.
The CLI opens both files without following links, requires owner-only regular
files owned by the current user, and refuses files larger than 1 MiB. The age
identity travels through an inherited file descriptor; decryption stays in
memory. Neither key material nor child-command diagnostics are printed. The
unseal request travels on kubectl stdin to OpenBao's API inside the verified
pod. The key is never an argument or environment variable. No root token is
needed. Failed unseal leaves the recovery file untouched.

`oberth unseal --status --json` reads only `initialized` and `sealed`. It opens
no recovery file and has a five-second timeout. A successful unseal rechecks
seal status independently. An already unsealed store needs no recovery read.
Only the installer's single-share recovery format is supported; this command
does not collect multiple Shamir shares interactively.

`/oberth:unseal` uses the same command and asks only for file paths. Never paste
the unseal key into a session. `system_status` reports the remedy
`sealed: run /oberth:unseal`. The plugin's SessionStart hint checks status through
the local CLI when available; when the CLI or a usable kubeconfig is absent,
it cannot detect a sealed store. The server may itself be unable to start while
sealed, so MCP status is not the recovery transport.

Optional [TPM2 boot unseal](../deploy/systemd/README.md) is explicit and off by
default. It refuses unsupported topology, a different host, managed providers,
and missing TPM2 support. Anyone who boots the disk on the bound machine gets
the secrets. Keep the original recovery file offline: changed TPM/PCR state
can prevent automatic decryption.
