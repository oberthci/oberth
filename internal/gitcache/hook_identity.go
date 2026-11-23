package gitcache

import (
	"crypto/sha256"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type trustedHook struct {
	mu     sync.Mutex
	digest [sha256.Size]byte
	info   os.FileInfo
}

// The configured hook is trusted at startup. Device/inode, size, mode, mtime
// and ctime identify unchanged bytes. A changed identity must hash to the same
// startup digest before it can become the fast path for later receives.
func (h *trustedHook) check(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var digest [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return hookUnavailable(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return hookUnavailable(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || unix.Access(path, unix.X_OK) != nil {
		return hookUnavailable(nil)
	}
	if sameHookFile(h.info, info) {
		return nil
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return hookUnavailable(err)
	}
	copy(digest[:], hash.Sum(nil))
	after, err := f.Stat()
	if err != nil || !sameHookFile(info, after) {
		return hookUnavailable(err)
	}
	if h.info != nil && digest != h.digest {
		return hookUnavailable(nil)
	}
	h.digest, h.info = digest, after
	return nil
}

func sameHookFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() &&
		a.ModTime().Equal(b.ModTime()) && hookChangeTime(a).Equal(hookChangeTime(b))
}

func (c *Cache) checkHook() error { return c.hookIdentity.check(c.config.HookBinary) }

type hookFailure struct{ cause error }

func (e *hookFailure) Error() string {
	message := reasonHook + ". " + remedyHook
	if e.cause != nil {
		message += " " + e.cause.Error()
	}
	return message
}
func hookUnavailable(cause error) error {
	return unavailable("receive hook", &hookFailure{cause: cause})
}

func (c *Cache) loadHookIdentity() error {
	if c.config.HookBinary == "" {
		return nil
	}
	c.hookIdentity = &trustedHook{}
	return c.checkHook()
}
