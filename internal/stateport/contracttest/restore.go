package contracttest

import (
	"bytes"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func concurrentRestores(t *testing.T, f Fixture) {
	t.Helper()
	body, _ := f.snapshot(t)
	request := f.restoreRequest(t)
	f.assertDestination(t, request.Destination, false)
	var group sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 16)
	for range cap(results) {
		group.Go(func() {
			<-start
			_, err := f.Port.Restore(t.Context(), request, bytes.NewReader(body))
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		assertCode(t, err, publicerr.Conflict)
	}
	if successes != 1 {
		t.Fatalf("successful restores = %d, want 1", successes)
	}
	f.assertDestination(t, request.Destination, true)
}
