package gitcache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// An impossible marker must not prevent a fresh fetch from repairing a cache.
// Busy native lock files and cancellation remain retryable in place.
func (c *Cache) setAsideReconciliation(store string, cause error) error {
	var invalid *reconciliationFailure
	var exit *exec.ExitError
	structural := errors.As(cause, &exit) &&
		(strings.Contains(cause.Error(), "exists; cannot create") ||
			strings.Contains(cause.Error(), "nonexistent object") || strings.Contains(cause.Error(), "directory not empty"))
	if !errors.As(cause, &invalid) && !structural {
		return cause
	}
	marker := filepath.Join(store, reconciliationFile)
	if err := os.Rename(marker, marker+".stuck"); err != nil {
		return err
	}
	if c.config.Log != nil {
		c.config.Log(reasonRecoveryAside, store+": "+cause.Error())
	}
	return nil
}
