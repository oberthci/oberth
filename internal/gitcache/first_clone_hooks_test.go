package gitcache

// WaitFirstClones waits for running first clones, including what each does
// after it ends, without ending the cache's lifetime.
func WaitFirstClones(c *Cache) { c.clones.wait.Wait() }
