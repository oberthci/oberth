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
			workers.Go(func() { serveStalledBody(ctx, remote, config, http.StatusOK) })
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

func serveStalledBody(ctx context.Context, remote net.Conn, config *tls.Config, status int) {
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
	_, _ = fmt.Fprintf(server, "HTTP/1.1 %d Status\r\nContent-Length: 10\r\nConnection: close\r\n\r\n", status)
	_, _ = io.Copy(io.Discard, server)
}

func TestStalledErrorBodyRefusesWithoutRetry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		apply(t, f, prepare(t, f))
		config := serverTLS(t, f)
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		defer func() { cancel(); workers.Wait() }()
		verifier := installer.EndpointVerifier{Now: config.Time, DialContext: func(context.Context, string, string) (net.Conn, error) {
			client, remote := net.Pipe()
			workers.Go(func() { serveStalledBody(ctx, remote, config, http.StatusInternalServerError) })
			return client, nil
		}}
		checks := 0
		f.engine.CheckEndpoint = func(ctx context.Context, profile installer.Profile) error {
			checks++
			profile.ServerURL = "https://server.oberth.ci"
			return verifier.Verify(ctx, profile)
		}
		p := prepare(t, f)
		timeline := clock.RealClock{}
		started := timeline.Now()
		_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "tls" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("stalled HTTP 500 body became transient", err)
		}
		if checks != 1 || timeline.Since(started) != 10*time.Second {
			t.Fatal("HTTP 500 was retried", checks, timeline.Since(started))
		}
	})
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
