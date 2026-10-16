# Host services

## OpenBao auto-unseal (`oberth-openbao-unseal`)

A production OpenBao installed by `oberth install --install-secretstore` seals
whenever its pod restarts, and Oberth cannot load its identities until someone
unseals it. On a single node where keeping the unseal key on that node is an
accepted risk, this service unseals it automatically.

The key stays root-only on disk and reaches the service as a systemd credential;
the script submits it to the pod on stdin, never in a command line.

```sh
sudo install -m 0755 deploy/systemd/oberth-openbao-unseal /usr/local/libexec/
sudo install -m 0644 deploy/systemd/oberth-openbao-unseal.service /etc/systemd/system/
sudo install -d -m 0700 /etc/oberth
sudo install -m 0400 /dev/stdin /etc/oberth/openbao-unseal-key   # paste the unseal key, then Ctrl-D
sudo systemctl daemon-reload
sudo systemctl enable --now oberth-openbao-unseal
journalctl -u oberth-openbao-unseal -f
```

The unit assumes k3s (`/etc/rancher/k3s/k3s.yaml`) and the default `openbao`
namespace and `openbao-0` pod; override `KUBECONFIG`, `OPENBAO_NAMESPACE`,
`OPENBAO_POD` or `OPENBAO_UNSEAL_INTERVAL` with a drop-in.
