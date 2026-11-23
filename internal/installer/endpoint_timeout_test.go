package installer_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"k8s.io/utils/clock"
)

func TestEndpointBodyTimeoutIsTransient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		profile := apply(t, f, prepare(t, f)).Profile
		profile.ServerURL = "https://server.oberth.ci"
		config := serverTLS(t, f)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var workers sync.WaitGroup
		verifier := installer.EndpointVerifier{Now: config.Time, DialContext: func(context.Context, string, string) (net.Conn, error) {
			client, remote := net.Pipe()
			workers.Go(func() { serveStalledBody(ctx, remote, config) })
			return client, nil
		}}
		timeline := clock.RealClock{}
		started := timeline.Now()
		err := verifier.Verify(ctx, profile)
		cancel()
		workers.Wait()
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "connect" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("body timeout was treated as a trust failure", err)
		}
		if timeline.Since(started) != 2*time.Minute {
			t.Fatal("request did not use its own deadline", timeline.Since(started))
		}
	})
}

func serveStalledBody(ctx context.Context, remote net.Conn, config *tls.Config) {
	stop := context.AfterFunc(ctx, func() { _ = remote.Close() })
	defer stop()
	pipe := bufferTLS(remote)
	defer func() { _ = pipe.Close() }()
	server := tls.Server(pipe, config)
	if err := server.HandshakeContext(ctx); err != nil {
		return
	}
	request, err := http.ReadRequest(bufio.NewReader(server))
	if err != nil {
		return
	}
	_ = request.Body.Close()
	_, _ = fmt.Fprint(server, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\nConnection: close\r\n\r\n")
	_, _ = io.Copy(io.Discard, server)
}

func TestLateReadyReplyCannotPassDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		waiting, cancel := context.WithTimeout(t.Context(), installer.ReadinessTimeout)
		defer cancel()
		f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
			<-waiting.Done()
			return nil
		}
		p := prepare(t, f)
		_, err := f.engine.Apply(waiting, p, p.Digest(), nil)
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "timeout" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("late readiness bypassed the deadline", err)
		}
	})
}
