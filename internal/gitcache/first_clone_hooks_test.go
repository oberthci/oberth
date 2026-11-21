package gitcache

import "time"

// SetFirstCloneBound replaces firstCloneBound for one cache.
func SetFirstCloneBound(c *Cache, d time.Duration) { c.budgets.firstClone = d }

// WaitFirstClones waits for running first clones, including what each does
// after it ends, without ending the cache's lifetime.
func WaitFirstClones(c *Cache) { c.clones.wait.Wait() }
