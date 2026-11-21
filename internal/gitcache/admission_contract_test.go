package gitcache_test

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/contracttest"
	"github.com/oberthci/oberth/internal/gitcache/fake"
	"testing"
)

func TestAdmissionCallbackCancellationContract(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	callback := fake.Admission{Entered: entered}
	contracttest.AdmissionCancellation(t,
		func(ctx context.Context) error {
			return callback.Check(ctx,
				gitcache.ReceiveEvent{},
				nil)
		},
		entered)
}
