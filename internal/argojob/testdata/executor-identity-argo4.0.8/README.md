# Executor identity Pod observations

`ordinary-pods.json` and `resource-pods.json` are the Pod lists read back from
a disposable kind cluster running Argo Workflows v4.0.8 (executor
`quay.io/argoproj/argoexec:v4.0.8`) after `TestLiveExecutorIdentityDefaulting`
completed both Workflows. They carry real API defaults (pull policy,
termination message fields) and public references only, never delivered token
data. `TestExecutorIdentityObservedAPIPods` replays them through the current
target validation without a cluster.

To regenerate, run the live test with `go test -artifacts -outputdir <dir>`
against a cluster from `hack/argo-live-cluster.sh` and copy each subtest's
`<shape>-pods.json` here.
