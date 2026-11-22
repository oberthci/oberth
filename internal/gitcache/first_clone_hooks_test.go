package gitcache

import "time"

// SetBackgroundBound replaces backgroundBound for one cache.
func SetBackgroundBound(c *Cache, d time.Duration) { c.budgets.background = d }

// SetRefreshWait replaces refreshDeadline, the wait of a refresh's caller,
// for one cache.
func SetRefreshWait(c *Cache, d time.Duration) { c.budgets.refreshWait = d }

// SetHeadWait replaces headDeadline, a pin's wait for the forge's default
// head, for one cache.
func SetHeadWait(c *Cache, d time.Duration) { c.budgets.headWait = d }

// CacheTrustWindow and the forge refusal reasons, for tests.
const (
	CacheTrustWindow   = cacheTrustWindow
	ReasonForgeSilent  = reasonForgeSilent
	RemedyForgeSilent  = remedyForgeSilent
	ReasonForgeRefused = reasonForgeRefused
	ReasonForgeFailed  = reasonForgeFailed
	RemedyForgeFailed  = remedyForgeFailed
)

// WaitBackground waits for running first clones and refresh fetches,
// including what each does after it ends, without ending the cache's
// lifetime.
func WaitBackground(c *Cache) { c.transfers.wait.Wait() }
