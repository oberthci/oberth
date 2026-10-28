package gitcache

import (
	"bytes"
	"fmt"
)

type boundedGitInventory struct{ bytes.Buffer }

func (b *boundedGitInventory) Write(p []byte) (int, error) {
	const limit = 32 << 20
	if len(p) > limit-b.Len() {
		return 0, fmt.Errorf("promotion tree inventory exceeds %d bytes", limit)
	}
	return b.Buffer.Write(p)
}
