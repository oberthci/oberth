# Pinned controller chart fixture

`argo-workflows-1.0.24.tgz` is the unchanged public chart downloaded from
`https://argoproj.github.io/argo-helm` with Helm4.2.3 `pull argo-workflows
--repo https://argoproj.github.io/argo-helm --version 1.0.24`. Its SHA256 is
`7b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50`
and size86344 bytes, matching the previously qualified controller render
`internal/argojob/testdata/nonroot-argo4.0.8/controller-profile.json`.

The full RunUpgrade tests copy these real bytes through the production digest
check, without a production test bypass or live network/cluster mutation.
They test mutation order and partial failures using fake Helm/Kubernetes
transports. Live before/after manifests and actual runtime identity remain
separate deployment qualifications.
