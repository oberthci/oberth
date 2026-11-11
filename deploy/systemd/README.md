# Host services

## TPM2-bound OpenBao auto-unseal

Off by default. **Anyone who boots this disk on this machine gets the secrets.**
Only enable this when that risk is acceptable. The selected cluster must have
exactly one node, matching this machine's ID, with a local k3s system service.
Managed providers and remote/multiple nodes are refused. A usable TPM2 and
systemd with encrypted credential support are required; there is no plaintext,
`host`, `auto` or `null` key fallback.

The operator runs these commands on the node as root, using a root-owned 0600
recovery file (and a root-owned age identity file if encrypted). Do not paste
any key into a terminal or agent session. Helm/kubectl remain prerequisites.
The Oberth CLI must already be installed at `/usr/local/bin/oberth`.

```sh
install -d -m 0700 /etc/oberth
oberth unseal --kubeconfig /etc/rancher/k3s/k3s.yaml \
  --secrets-file '<root-owned recovery file>' \
  --seal-to-host /etc/oberth/openbao-unseal-key.cred --yes
install -m 0755 deploy/systemd/oberth-openbao-unseal /usr/local/libexec/
install -m 0644 deploy/systemd/oberth-openbao-unseal.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now oberth-openbao-unseal
```

The setup command writes only a new encrypted credential and never overwrites
one. `systemd-creds encrypt --with-key=tpm2` receives the recovery value on stdin.
The unit uses `LoadCredentialEncrypted`, decrypts into private runtime memory,
and calls `oberth unseal` after k3s starts. It retries failed starts and exits
when unsealed. It submits no key in argv or environment and logs seal status
only. Configure the kubeconfig/context or namespace with a unit drop-in when
needed; verify the chosen target before enabling it.

Keep the original recovery file offline. TPM reset, firmware/Secure Boot changes
or PCR changes may require manual unseal and a new encrypted credential. This
uses systemd's default PCR binding (PCR 7), not a claim that every boot is safe.
See [systemd credential encryption](https://www.freedesktop.org/software/systemd/man/systemd-creds.html).

Hardware qualification requires a TPM-capable VM or physical node. The isolated
unit tests exercise no-fallback behavior and refusal; they do not establish
successful TPM encryption or a reboot round trip on real hardware.
