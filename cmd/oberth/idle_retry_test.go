package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func idleResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"X-Oberth-Idle-Version": {"1"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestUpgradeWaitRetriesTimeoutBeforeUpgrade(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls, upgrades := 0, 0
				client := &http.Client{Timeout: 5 * time.Second, Transport: idleRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 || persistent {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return idleResponse(204, ""), nil
				})}
				var output bytes.Buffer
				upgradeReached := errors.New("fixture reached Helm upgrade")
				deps := installer.Deps{
					Output: &output,
					RunCommandStream: func(ctx context.Context, w io.Writer, name string, args ...string) error {
						if name != "kubectl" || !slices.Contains(args, "--wait-for=critical") {
							t.Fatalf("unexpected command %s %v", name, args)
						}
						return waitIdle(ctx, client, "http://fixture/v1/idle?wait_for=critical", time.Second, idleWaitOptions{mode: "critical", output: w})
					},
					RunHelm: func(_ context.Context, args []string) ([]byte, error) {
						switch args[0] {
						case "list":
							return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.17.66"}]`), nil
						case "version":
							return []byte("v4.2.3"), nil
						case "show":
							return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("a", 64) + "\n"), nil
						case "history":
							return []byte(`[{"revision":1,"status":"deployed"}]`), nil
						case "get":
							if slices.Contains(args, "--all") {
								return []byte(`{"compatibility":{"schemaIdentity":"legacy-schema-v1","auditDomain":"legacy-audit-v1","witnessKeyInfo":"legacy-witness-v1"}}`), nil
							}
							return []byte(`{}`), nil
						case "template":
							return nil, nil
						case "upgrade":
							upgrades++
							return nil, upgradeReached
						default:
							return nil, fmt.Errorf("unexpected Helm command %v", args)
						}
					},
				}
				_, err := installer.RunUpgrade(t.Context(), installer.UpgradeConfig{BinaryVersion: "0.17.67", ChartOverride: "/fixture", Yes: true, WaitFor: "critical", WaitTimeout: 2 * time.Minute}, deps)
				if persistent {
					if upgrades != 0 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "last idle probe failure: read live daemon queue") || !strings.Contains(err.Error(), "upgrade not attempted") || time.Since(start) != 2*time.Minute {
						t.Fatalf("upgrades=%d elapsed=%s err=%v", upgrades, time.Since(start), err)
					}
					if n := strings.Count(output.String(), "transient idle probe failure:"); n != 2 {
						t.Fatalf("got %d diagnostics, want two: %s", n, &output)
					}
				} else if !errors.Is(err, upgradeReached) || upgrades != 1 || calls != 2 || time.Since(start) != 6*time.Second {
					t.Fatalf("upgrades=%d calls=%d elapsed=%s err=%v", upgrades, calls, time.Since(start), err)
				}
			})
		})
	}
}

type idleBrokenBody struct{}

func (idleBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (idleBrokenBody) Close() error             { return nil }

func TestWaitIdleTransientFailuresBackOffAndRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
			attempts = append(attempts, time.Since(start))
			switch len(attempts) {
			case 1:
				return nil, errors.New("connection refused")
			case 2:
				return idleResponse(503, "unavailable"), nil
			case 3:
				response := idleResponse(409, "")
				response.Body = idleBrokenBody{}
				return response, nil
			case 4:
				return idleResponse(409, `{"version":1,"queued":1,"tag":1}`), nil
			case 5:
				return nil, errors.New("connection reset")
			default:
				return idleResponse(204, ""), nil
			}
		})}
		var output bytes.Buffer
		if err := waitIdle(t.Context(), client, "http://fixture", time.Second, idleWaitOptions{mode: "critical", output: &output}); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 8 * time.Second, 9 * time.Second}
		if !slices.Equal(attempts, want) || strings.Count(output.String(), "transient idle probe failure:") != 1 {
			t.Fatalf("attempts=%v output=%s", attempts, &output)
		}
	})
}

func TestWaitIdleProtocolErrorsFailImmediately(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"auth", 401, ""}, {"forbidden", 403, ""}, {"not found", 404, ""}, {"bad request", 400, ""},
		{"invalid JSON", 409, "{"}, {"invalid schema", 409, `{"version":2}`}, {"missing snapshot", 409, ""},
		{"oversized", 409, strings.Repeat(" ", 4097)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return idleResponse(test.code, test.body), nil
			})}
			if err := waitIdle(t.Context(), client, "http://fixture", time.Second, idleWaitOptions{mode: "critical"}); err == nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWaitIdleCancellationInterruptsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(500*time.Millisecond, cancel)
		cause := errors.New("fixture dial failed")
		client := &http.Client{Transport: idleRoundTrip(func(*http.Request) (*http.Response, error) { return nil, cause })}
		start := time.Now()
		err := waitIdle(ctx, client, "http://fixture", time.Second)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || time.Since(start) != 500*time.Millisecond {
			t.Fatalf("elapsed=%s err=%v", time.Since(start), err)
		}
	})
}
