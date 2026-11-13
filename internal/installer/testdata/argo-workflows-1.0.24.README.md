# Pinned controller chart fixture

`argo-workflows-1.0.24.tgz` is the unchanged public chart downloaded from
`https://argoproj.github.io/argo-helm` with Helm4.2.3 `pull argo-workflows
--repo https://argoproj.github.io/argo-helm --version 1.0.24`. Its SHA256 is
`7b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50`
and size 86344 bytes.

Installer tests render this pinned chart with the actual native Helm arguments.
Live workload acceptance remains separate deployment qualification.
