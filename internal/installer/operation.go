package installer

import (
	"os"
	"syscall"
)

func (c *ConfigDir) lock() (func(), error) {
	if !c.operation.TryLock() {
		return nil, problem("operation", nil)
	}
	if err := flock(c.directory, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		c.operation.Unlock()
		return nil, err
	}
	c.operationHeld = true
	return func() {
		_ = flock(c.directory, syscall.LOCK_UN)
		c.operationHeld = false
		c.operation.Unlock()
	}, nil
}

// A fresh directory is created and locked only after read-only revalidation.
func flock(directory *os.File, operation int) error {
	if directory == nil {
		return nil
	}
	if err := syscall.Flock(int(directory.Fd()), operation); err != nil {
		return problem("operation", err)
	}
	return nil
}
