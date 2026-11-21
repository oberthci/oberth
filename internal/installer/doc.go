// Package installer turns reviewed choices into one chart release and a private
// administrator profile. Kubernetes and chart operations are injected; preparation
// is read-only and apply rechecks the target before saving credentials or writing.
//
// Spec: docs/plans/c9-install-and-upgrade.md#purpose
package installer
