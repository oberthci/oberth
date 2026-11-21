// Package sourceplugin atomically stages receipt-verified read-only input archives for Argo.
//
// Executor configuration uses OBERTH_SOURCE_ENDPOINT, OBERTH_SOURCE_SERVER_NAME
// and OBERTH_SOURCE_CA_PEM. The plugin reads the native executor token at
// /var/run/secrets/kubernetes.io/serviceaccount/token.
// Every HTTP attempt reopens that file; projected-volume symlinks must stay inside
// its directory so token rotation works without accepting a symlink escape.
//
// Spec: docs/plans/c6-source-delivery.md#owned-data-and-public-contract
package sourceplugin
