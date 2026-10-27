//go:build linux

package gitcache

import "os"

// Increasing our own OOM score requires no additional capability. If the
// runtime forbids this, skip the observer rather than run with server priority.
func prepareShadowHelper() error {
	return os.WriteFile("/proc/self/oom_score_adj", []byte("1000\n"), 0)
}
