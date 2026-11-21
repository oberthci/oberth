// Package runlog retains native container spans and reads bounded plain output.
// It records capture gaps without deriving execution status from log content.
// Step/attempt reads select authored containers marked Main. Explicit
// container selection and whole-run reads include the requested containers.
//
// Spec: docs/plans/c8-logs.md#owned-data-and-public-contract
package runlog
