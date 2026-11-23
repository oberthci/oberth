package gitcache

import "time"

// SetAdmissionTimeout changes one cache's budget between receives and returns
// a function that restores it. No receive may run while the budget changes.
func SetAdmissionTimeout(c *Cache, timeout time.Duration) func() {
	before := c.config.AdmissionTimeout
	c.config.AdmissionTimeout = timeout
	return func() { c.config.AdmissionTimeout = before }
}
