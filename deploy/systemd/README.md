# Host services

## Private recovery

After a production OpenBao restarts, use `/oberth:unseal` or:

```sh
oberth unseal --context '<user-selected-context>' --secrets-file /secure/install-secrets
```

The installer file must be a regular owner-only file (0600), not a symlink.
The CLI reads `OPENBAO_UNSEAL_KEY` without sourcing the file and submits JSON
to OpenBao's `sys/unseal` API through kubectl stdin. It prints only seal status.
No key is put on argv, in the environment, or in command error output.
Already unsealed stores need no credential read. Custom installations can use
`--namespace` and `--pod`. kubectl is a user prerequisite; install it from
https://kubernetes.io/docs/tasks/tools/ if missing.

For an age-encrypted installer file, also give `--age-identity /secure/identity`.
Install age yourself if needed. Only the identity's path is an argument;
decryption happens over stdin and plaintext is never written to disk.
The plugin must never open either secret file or ask for key material in chat.

## Optional TPM2 host-bound auto-unseal

Off by default. Anyone who boots the disk on this machine gets the secrets.
This trades operator intervention for recovery on that same host. It is refused
without Linux, systemd TPM2 support, exactly one node with this host's machine
ID, and no cloud provider ID (a local `k3s://` node ID is accepted). Do not offer it for multi-node or managed clusters.

After the user explicitly accepts that risk, run on the actual cluster host:

```sh
sudo install -d -m 0700 /etc/oberth
sudo /usr/local/bin/oberth unseal --context '<user-selected-context>' \
  --secrets-file /secure/install-secrets \
  --seal-to-tpm2 /etc/oberth/unseal.cred --acknowledge-host-risk
sudo install -m 0755 deploy/systemd/oberth-openbao-unseal /usr/local/libexec/
sudo install -m 0644 deploy/systemd/oberth-openbao-unseal.service /etc/systemd/system/
sudo systemctl edit oberth-openbao-unseal
```

The encryption uses `systemd-creds encrypt --with-key=tpm2 --tpm2-device=auto`;
there is no disk-only fallback. Only the unseal key is encrypted, with the
credential name `oberth-secrets`; other ceremony secrets are excluded.
Existing credential files are refused. Keep the original recovery file offline.

In the service drop-in, set public coordinates and a root-readable kubeconfig:

```ini
[Service]
Environment=KUBECONFIG=/etc/oberth/kubeconfig
Environment=OPENBAO_CONTEXT=<user-selected-context>
```

Copy the selected kubeconfig securely to that location yourself with mode
0600. Override `OPENBAO_NAMESPACE` and `OPENBAO_POD` if necessary. Then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now oberth-openbao-unseal
```

The unit runs after the node service and retries failures until the API and
pod are reachable. systemd decrypts the TPM2 credential into its private
runtime credential directory; the service calls the same private CLI path.
It exits after unsealing. For a later pod restart, start the unit again.
Never replace `LoadCredentialEncrypted` with a plaintext credential.
