package gitcache

import "time"

func DeltaSetNativeIdle(c *Cache, d time.Duration) { c.budgets.idle = d }
