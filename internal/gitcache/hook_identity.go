package gitcache

import (
	"crypto/sha256"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// The configured hook is trusted at startup and must retain its identity for
// every receive. Git itself skips missing or non-executable hook programs.
func hookIdentity(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return digest, hookUnavailable(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return digest, hookUnavailable(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || unix.Access(path, unix.X_OK) != nil {
		return digest, hookUnavailable(nil)
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return digest, hookUnavailable(err)
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}
func (c *Cache) checkHook() error {
	digest, err := hookIdentity(c.config.HookBinary)
	if err != nil {
		return err
	}
	if digest != c.hookDigest {
		return hookUnavailable(nil)
	}
	return nil
}

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
	digest, err := hookIdentity(c.config.HookBinary)
	c.hookDigest = digest
	return err
}
