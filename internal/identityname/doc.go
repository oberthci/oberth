// Package identityname defines deterministic identity naming.
// Names fit within 63 bytes and use 20 hex digits (80 bits) of SHA-256, requiring
// about 2^80 hashes to target a fixed identity.
//
// Spec: docs/plans/README.md#shared-contracts
package identityname
