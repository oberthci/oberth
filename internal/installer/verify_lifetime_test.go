package installer_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/installer"
)

func TestVerifierJoinsCanceledDialBeforeReturning(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		profile := apply(t, f, prepare(t, f)).Profile
		entered, release := make(chan struct{}), make(chan struct{})
		verifier := installer.EndpointVerifier{Now: f.engine.Now, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(entered)
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- verifier.Verify(ctx, profile) }()
		<-entered
		cancel()
		synctest.Wait()
		var err error
		returned := false
		select {
		case err = <-done:
			returned = true
		default:
		}
		close(release)
		if !returned {
			err = <-done
		}
		if returned {
			t.Fatal("verification returned while its dial callback was still running")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal("verification lost request cancellation", err)
		}
	})
}
