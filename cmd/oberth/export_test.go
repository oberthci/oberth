package oberth

import (
	"context"
	"io"
	"time"
)

// runReleaseTierVerify proves the release-tier OpenBao trust chain end to end.
// The server tier (secretstore verify without --release-tier) verifies the
// server's own leg: its own namespace, ServiceAccount, and secretstore.caCert.
// The release tier runs in a completely independent namespace with a different
// ServiceAccount and a separate CA anchor; nothing automated exercises this
// path until a real release tag runs a credentialed step.
//
// The approach:
//  1. Discover --argo-vault-*, --argo-credentialed-serviceaccount, and --argo-namespace
//     from the serve process command line.
//  2. Request a short-lived token for the credentialed ServiceAccount via the
//     Kubernetes TokenRequest API (the server's SA must have create permission
//     on serviceaccounts/token in the pipeline namespace).
//  3. Attempt a real Vault Kubernetes-auth login with that token, against
//     --argo-vault-address, using --argo-vault-ca-cert as the CA anchor.
//  4. Fetch at least one path from the server's --secretstore-path allowlist
//     to prove read access.
func runReleaseTierVerify(
	ctx context.Context,
	explicit secretStoreVerifyConfig,
	positionalPaths []string,
	repo string,
	tier string,
	timeout time.Duration,
	output io.Writer,
	procCmdline string,
	newKube kubeClientFactory,
) error {
	return runReleaseTierVerifyWithSources(ctx, explicit, positionalPaths, repo, tier, timeout, output, secretStoreVerifySourcesFromCmdline(procCmdline, newKube))
}
