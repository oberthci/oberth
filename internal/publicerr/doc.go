// Package publicerr defines public failure reasons and remedies with private
// causes. Invariant dominates; otherwise the public refusal nearest the caller
// supplies the code (join order breaks equal-depth ties), and all reason/remedy
// pairs with that code are kept. An unlabeled leaf or typed nil is invariant.
//
// Spec: docs/plans/README.md#shared-contracts
package publicerr
