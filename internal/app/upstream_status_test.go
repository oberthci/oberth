package app

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/oberthci/oberth/internal/model"
)

func TestUpstreamFailureThresholdAndBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstreams := []model.Upstream{{ID: 1, Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/first"}}
		calls, failing := 0, true
		health := Health{VCSCache: &VCSSnapshot{}, VCS: func(context.Context, model.Upstream) error {
			calls++
			if failing {
				return errors.New("handshake timeout")
			}
			return nil
		}}
		for i, delay := range []time.Duration{2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
			result := health.probeVCS(t.Context(), upstreams)[0]
			want := "checking"
			if i >= 2 {
				want = "unavailable"
			}
			if result.Probe != want || result.ConsecutiveFailures != i+1 || result.LastError != "handshake timeout" || result.CheckedAt.IsZero() {
				t.Fatalf("failure %d: %+v", i+1, result)
			}
			time.Sleep(delay - time.Nanosecond)
			if got := health.probeVCS(t.Context(), upstreams)[0]; got != result || calls != i+1 {
				t.Fatal("backoff changed observation or opened transport", got, calls)
			}
			time.Sleep(time.Nanosecond)
		}
		failing = false
		if result := health.probeVCS(t.Context(), upstreams)[0]; result.Probe != "ready" || result.LastError != "" || result.ConsecutiveFailures != 0 {
			t.Fatal("recovery did not clear the failure streak", result)
		}
		failing = true
		time.Sleep(time.Minute)
		result := health.probeVCS(t.Context(), upstreams)[0]
		if result.Probe != "ready" || result.ConsecutiveFailures != 1 || result.LastError == "" {
			t.Fatal("one transient failure erased last healthy state", result)
		}
		if got := vcsState([]UpstreamStatus{result}); got != "ready" {
			t.Fatal(got)
		}
	})
}

func TestUpstreamSharingKeepsCredentialAndEndpointBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstreams := []model.Upstream{
			{ID: 1, Kind: "ssh", BaseURL: "ssh://git@forge.example/first", KeyName: "shared"},
			{ID: 2, Kind: "github", BaseURL: "ssh://git@forge.example:22/second", KeyName: "shared"},
			{ID: 3, Kind: "ssh", BaseURL: "ssh://git@forge.example/third", KeyName: "other"},
			{ID: 4, Kind: "ssh", BaseURL: "ssh://user@forge.example/first", KeyName: "shared"},
			{ID: 5, Kind: "ssh", BaseURL: "ssh://git@forge.example:2222/first", KeyName: "shared"},
			{ID: 6, Kind: "ssh", BaseURL: "ssh://git@[fe80::1%25clusterA]/first", KeyName: "shared"},
			{ID: 7, Kind: "ssh", BaseURL: "ssh://git@[fe80::1%25clustera]/first", KeyName: "shared"},
			{ID: 8, Kind: "local", BaseURL: "/srv/first"},
			{ID: 9, Kind: "local", BaseURL: "/srv/second"},
			{ID: 10, Kind: "ssh", BaseURL: "ssh://git@FORGE.example/first", KeyName: "shared"},
			{ID: 11, Kind: "ssh", BaseURL: "ssh://git@forge.example:022/first", KeyName: "shared"},
		}
		var calls atomic.Int32
		health := Health{VCSCache: &VCSSnapshot{}, VCS: func(context.Context, model.Upstream) error { calls.Add(1); return nil }}
		results := health.probeVCS(t.Context(), upstreams)
		if calls.Load() != 10 || len(results) != 11 {
			t.Fatal("unexpected endpoint sharing", calls.Load(), results)
		}
		if results[0].CheckedAt != results[1].CheckedAt || results[0].ID == results[1].ID {
			t.Fatal("shared sample lost per-upstream identity")
		}
		// Changing the credential binding causes an immediate new observation;
		// unchanged endpoints retain their retry clocks and checked_at values.
		upstreams[0].KeyName = "rotated"
		health.probeVCS(t.Context(), upstreams)
		if calls.Load() != 11 {
			t.Fatal("credential change reused a prior sample", calls.Load())
		}
	})
}

func TestUpstreamConcurrentRefreshSharesOneAttemptAndCancellationKeepsEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		upstreams := []model.Upstream{{ID: 1, Kind: "ssh", BaseURL: "ssh://git@forge.example/org"}}
		health := Health{VCSCache: &VCSSnapshot{}, VCS: func(ctx context.Context, _ model.Upstream) error {
			calls.Add(1)
			select {
			case <-time.After(time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		var workers sync.WaitGroup
		for range 10 {
			workers.Go(func() { health.probeVCS(t.Context(), upstreams) })
		}
		synctest.Wait()
		time.Sleep(time.Second)
		workers.Wait()
		if calls.Load() != 1 {
			t.Fatal("concurrent refresh opened duplicate transports", calls.Load())
		}
		before := health.vcsStatus(upstreams)[0]
		time.Sleep(time.Minute)
		ctx, cancel := context.WithCancel(t.Context())
		workers.Go(func() { health.probeVCS(ctx, upstreams) })
		synctest.Wait()
		cancel()
		workers.Wait()
		if after := health.vcsStatus(upstreams)[0]; after != before {
			t.Fatal("shutdown recorded a probe failure", after)
		}
	})
}

func TestFailingEndpointDoesNotBackOffHealthyEndpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var bad, good atomic.Int32
		upstreams := []model.Upstream{{ID: 1, Kind: "ssh", BaseURL: "ssh://git@bad.example/org"}, {ID: 2, Kind: "ssh", BaseURL: "ssh://git@good.example/org"}}
		health := Health{VCSCache: &VCSSnapshot{}, VCS: func(_ context.Context, u model.Upstream) error {
			if u.ID == 1 {
				bad.Add(1)
				return errors.New("timeout")
			}
			good.Add(1)
			return nil
		}}
		health.probeVCS(t.Context(), upstreams)
		time.Sleep(time.Minute)
		health.probeVCS(t.Context(), upstreams)
		if bad.Load() != 1 || good.Load() != 2 {
			t.Fatal("unrelated endpoint was backed off", bad.Load(), good.Load())
		}
	})
}

func TestUpstreamCycleDeadlineStillPublishesFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstreams := []model.Upstream{{ID: 1, Kind: "ssh", BaseURL: "ssh://git@forge.example/org"}}
		health := Health{VCSCache: &VCSSnapshot{}, StatusProbes: StatusProbeConfig{FailureThreshold: 1},
			VCS: func(ctx context.Context, _ model.Upstream) error { <-ctx.Done(); return ctx.Err() }}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result := health.probeVCS(ctx, upstreams)
		if len(result) != 1 || result[0].Probe != "unavailable" || result[0].LastError == "" || result[0].CheckedAt.IsZero() {
			t.Fatal("cycle deadline discarded the failure observation", result)
		}
	})
}

// A single known_hosts file can assign different keys to spellings of the same
// network endpoint. Sharing must preserve the decision of each real handshake.
func TestUpstreamSharingPreservesHostPinDecisions(t *testing.T) {
	client, private := testClientIdentity(t)
	address, originalPins := startGitShellForge(t, client, nil)
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	_, _, actualHost, _, _, err := ssh.ParseKnownHosts(originalPins)
	if err != nil {
		t.Fatal(err)
	}
	wrongHost, _ := testClientIdentity(t)
	good := net.JoinHostPort("localhost", port)
	for _, tc := range []struct {
		name, other string
		accepted    bool
	}{
		{"host case", net.JoinHostPort("LOCALHOST", port), false},
		{"port spelling", net.JoinHostPort("localhost", "0"+port), false},
		{"same spelling", good, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pins := []byte(knownhosts.Line([]string{good}, actualHost) + "\n")
			if !tc.accepted {
				pins = append(pins, []byte(knownhosts.Line([]string{tc.other}, wrongHost)+"\n")...)
			}
			upstreams := []model.Upstream{
				{ID: 1, Kind: "ssh", BaseURL: "ssh://git@" + good + "/first", KeyName: "shared"},
				{ID: 2, Kind: "ssh", BaseURL: "ssh://git@" + tc.other + "/second", KeyName: "shared"},
			}
			for _, u := range upstreams {
				err := ProbeSSHAuthentication(t.Context(), u.BaseURL, private, pins)
				if u.ID == 1 || tc.accepted {
					if err != nil {
						t.Fatalf("accepted endpoint: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "knownhosts: key mismatch") {
					t.Fatalf("different pin should reject endpoint, got %v", err)
				}
			}
			for _, reverse := range []bool{false, true} {
				name := "accepted first"
				ordered := append([]model.Upstream(nil), upstreams...)
				if reverse {
					name = "other first"
					ordered[0], ordered[1] = ordered[1], ordered[0]
				}
				t.Run(name, func(t *testing.T) {
					var calls atomic.Int32
					health := Health{VCSCache: &VCSSnapshot{}, StatusProbes: StatusProbeConfig{FailureThreshold: 1},
						PrepareVCS: func(_ context.Context, u model.Upstream) (string, func(context.Context) error, error) {
							return "same credential and pins", func(ctx context.Context) error {
								calls.Add(1)
								return ProbeSSHAuthentication(ctx, u.BaseURL, private, pins)
							}, nil
						}}
					results := health.probeVCS(t.Context(), ordered)
					wantCalls := int32(2)
					if tc.accepted {
						wantCalls = 1
					}
					if calls.Load() != wantCalls {
						t.Errorf("got %d probes, want %d", calls.Load(), wantCalls)
					}
					for _, result := range results {
						if result.ID == 1 || tc.accepted {
							if result.Probe != "ready" || result.LastError != "" {
								t.Errorf("accepted endpoint: %+v", result)
							}
						} else if result.Probe != "unavailable" || !strings.Contains(result.LastError, "knownhosts: key mismatch") {
							t.Errorf("rejected endpoint lost its pin decision: %+v", result)
						}
					}
					before := calls.Load()
					health.probeVCS(t.Context(), ordered)
					if calls.Load() != before {
						t.Fatal("opened another connection before next cycle")
					}
				})
			}
		})
	}
}
