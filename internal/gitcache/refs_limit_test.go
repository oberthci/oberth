package gitcache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type overflowingRefsBackend struct {
	canceled bool
	bytes    int
}

func (b *overflowingRefsBackend) execute(ctx context.Context, spec commandSpec, _ bool) (string, error) {
	chunk := []byte(strings.Repeat("x", 32<<10))
	for i := 0; i < maxRefsOutput/len(chunk)+2; i++ {
		n, err := spec.stdout.Write(chunk)
		b.bytes += n
		if err != nil {
			b.canceled = ctx.Err() != nil
			return "", err
		}
	}
	return "", errors.New("oversized output was fully buffered")
}

func TestRefListingBoundsWritesAndCancelsCommandOnOverflow(t *testing.T) {
	for _, boundedCount := range []bool{false, true} {
		backend := &overflowingRefsBackend{}
		c := &Cache{backend: backend, timeout: time.Minute}
		var err error
		if boundedCount {
			_, err = c.listRefsAtMost(t.Context(), "fixture", 100, "refs/heads/")
		} else {
			_, err = c.listRefs(t.Context(), "fixture", "refs/heads/")
		}
		if err == nil || !strings.Contains(err.Error(), "ref output exceeds") || backend.bytes > maxRefsOutput || !backend.canceled {
			t.Fatalf("bounded=%v bytes=%d canceled=%v err=%v", boundedCount, backend.bytes, backend.canceled, err)
		}
	}
}
