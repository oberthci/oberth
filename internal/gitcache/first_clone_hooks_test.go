package gitcache

import "time"

// SetBackgroundBound replaces backgroundBound for one cache.
func SetBackgroundBound(c *Cache, d time.Duration) { c.budgets.background = d }

// SetRefreshWait replaces refreshDeadline, the wait of a refresh's caller,
// for one cache.
func SetRefreshWait(c *Cache, d time.Duration) { c.budgets.refreshWait = d }

// WaitBackground waits for running first clones and refresh fetches,
// including what each does after it ends, without ending the cache's
// lifetime.
func WaitBackground(c *Cache) { c.transfers.wait.Wait() }
