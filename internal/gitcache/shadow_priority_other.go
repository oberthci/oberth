//go:build !linux

package gitcache

// Other supported development hosts have no Linux OOM preference interface.
// The launcher still starts the helper through nice before the Go runtime starts.
func prepareShadowHelper() error { return nil }
