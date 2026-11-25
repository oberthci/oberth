package installer_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestURLChangeRequiresThePinnedNewHostCertificate(t *testing.T) {
	t.Parallel()
	for _, scenario := range []rolloutScenario{
		{name: "new host", host: "new.oberth.ci", want: rolloutCounts{1, 0, 1}},
		{name: "starting", host: "new.oberth.ci", starting: true, want: rolloutCounts{2, 1, 1}},
		{name: "connecting", host: "new.oberth.ci", connecting: true, want: rolloutCounts{2, 1, 1}},
		{name: "bounded readiness", host: "new.oberth.ci", starting: true, deadline: true, code: "timeout", want: rolloutCounts{1, 1, 1}},
		{name: "trusted old host", host: "server.oberth.ci", want: rolloutCounts{2, 1, 1}},
		{name: "trusted old host bounded", host: "server.oberth.ci", deadline: true, code: "timeout", want: rolloutCounts{1, 1, 1}},
		{name: "untrusted old host", host: "server.oberth.ci", untrusted: true, code: "tls", want: rolloutCounts{1, 0, 1}},
		{name: "untrusted new host", host: "new.oberth.ci", untrusted: true, code: "tls", want: rolloutCounts{1, 0, 1}},
		{name: "unrelated host", host: "unrelated.oberth.ci", code: "tls", want: rolloutCounts{1, 0, 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) { urlRolloutCase(t, scenario) })
	}
}

type rolloutCounts struct{ checks, pauses, notices int }

type rolloutScenario struct {
	name, host, code                          string
	starting, connecting, deadline, untrusted bool
	want                                      rolloutCounts
}

func rolloutCertificate(t *testing.T, f fixture, scenario rolloutScenario) *tls.Config {
	t.Helper()
	if scenario.untrusted {
		f = setup(t)
		apply(t, f, prepare(t, f))
	}
	return serverTLSForName(t, f, scenario.host)
}

func urlRolloutCase(t *testing.T, scenario rolloutScenario) {
	t.Helper()
	f := setup(t)
	f.choices.URL = "https://server.oberth.ci"
	apply(t, f, prepare(t, f))
	certificate := rolloutCertificate(t, f, scenario)
	newCertificate := serverTLSForName(t, f, "new.oberth.ci")
	var rolled atomic.Bool
	var workers sync.WaitGroup
	defer workers.Wait()
	verifier := installer.EndpointVerifier{Now: f.engine.Now, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if scenario.connecting && !rolled.Load() {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		status := http.StatusOK
		if scenario.starting && !rolled.Load() {
			status = http.StatusServiceUnavailable
		}
		client, remote := net.Pipe()
		served := certificate
		if rolled.Load() {
			served = newCertificate
		}
		workers.Go(func() {
			serveTLSCase(ctx, remote, served, tlsCase{status: status, body: `{"ready":true,"version":"unset"}`})
		})
		return client, nil
	}}
	var counts rolloutCounts
	f.engine.CheckEndpoint = func(ctx context.Context, p installer.Profile) error {
		counts.checks++
		return verifier.Verify(ctx, p)
	}
	f.engine.Pause = func(context.Context) error {
		counts.pauses++
		if scenario.deadline || scenario.code == "tls" {
			return context.DeadlineExceeded
		}
		rolled.Store(true)
		return nil
	}
	f.choices.URL = "https://new.oberth.ci"
	p := prepare(t, f)
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if strings.HasPrefix(string(phase), "waiting for server") {
			counts.notices++
		}
		return nil
	})
	assertURLRollout(t, scenario, err, counts)
}

func assertURLRollout(t *testing.T, scenario rolloutScenario, err error, counts rolloutCounts) {
	t.Helper()
	if scenario.code == "" {
		if err != nil {
			t.Fatal("trusted new-host readiness did not complete", err)
		}
	} else {
		var p *installer.Problem
		if !errors.As(err, &p) || p.Code != scenario.code {
			t.Fatal("certificate refusal or readiness bound changed", scenario.name, err)
		}
	}
	if counts != scenario.want {
		t.Fatal("certificate rollout probe count changed", scenario.name, counts, scenario.want)
	}
}
