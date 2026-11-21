// Package restartretry bounds reconnect delays and waits for idempotent public
// observations. Callers use the tool registry's read classification before retry.
//
// Spec: docs/plans/c11-cli-and-server-composition.md#client-transport-and-checking
package restartretry
