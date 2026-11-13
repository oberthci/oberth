package installer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

type policyHTTPSpan struct {
	release        chan struct{}
	started, ended bool
}

// The HTTP server has no Kubernetes client and never sees policy or source
// identity. It sheds NEW requests while one long response occupies its capacity.
// Traffic and token start/end bytes are real localhost HTTP; only Kubernetes exec
// and marker-file transport are bridged to local calls/byte observations.
func TestPolicyRealHTTPConnectionPressureAndStall(t *testing.T) {
	for _, mode := range []string{"new-connection-reset", "held-response-stalls", "readiness-flap"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			spans := map[string]*policyHTTPSpan{}
			var holding, workers, ordinary, resets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/hold" {
					token := r.URL.RawQuery
					s := &policyHTTPSpan{release: make(chan struct{})}
					mu.Lock()
					spans[token] = s
					mu.Unlock()
					holding.Add(1)
					defer holding.Add(-1)
					fmt.Fprintf(w, "oberth-control-start:%s\n%s", token, strings.Repeat("x", 16*1024))
					w.(http.Flusher).Flush()
					select {
					case <-s.release:
					case <-r.Context().Done():
						return
					}
					if mode == "held-response-stalls" {
						<-r.Context().Done()
						return
					}
					fmt.Fprintf(w, "\noberth-control-end:%s\n", token)
					return
				}
				ordinary.Add(1)
				if holding.Load() > 0 && mode != "held-response-stalls" {
					resets.Add(1)
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						return
					}
					if tcp, ok := c.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
					_ = c.Close()
					return
				}
				fmt.Fprint(w, "oberth-policy-probe")
			}))
			defer server.Close()
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			httpClient := &http.Client{Transport: transport}
			kube := fake.NewClientset(preflightNode())
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			var cleanupUID atomic.Bool
			kube.PrependReactor("delete", "namespaces", func(a kt.Action) (bool, runtime.Object, error) {
				d := a.(kt.DeleteAction)
				p := d.GetDeleteOptions().Preconditions
				cleanupUID.Store(p != nil && p.UID != nil && *p.UID != "")
				return false, nil, nil
			})
			deps := Deps{KubeClient: kube, RunCommand: func(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
				workers.Add(1)
				defer workers.Add(-1)
				ns, _, script, values := policyModelArgs(args)
				switch script {
				case policyOverlapReadyScript:
					mu.Lock()
					defer mu.Unlock()
					s := spans[values[0]]
					if s == nil || !s.started {
						return []byte("pending"), nil
					}
					if s.ended {
						return []byte("closed"), nil
					}
					return []byte("open"), nil
				case policyOverlapReleaseScript:
					mu.Lock()
					s := spans[values[0]]
					if s != nil {
						close(s.release)
					}
					mu.Unlock()
					return []byte("released"), nil
				case policyOverlapScript:
					token := values[1]
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/hold?"+token, nil)
					resp, err := httpClient.Do(req)
					if err != nil {
						return nil, err
					}
					defer resp.Body.Close()
					reader := bufio.NewReader(resp.Body)
					line, err := reader.ReadString('\n')
					if err != nil {
						return nil, err
					}
					if line != "oberth-control-start:"+token+"\n" {
						return []byte("invalid"), nil
					}
					mu.Lock()
					spans[token].started = true
					mu.Unlock()
					rest, err := io.ReadAll(reader)
					if err != nil {
						return nil, err
					}
					if !strings.Contains(string(rest), "\noberth-control-end:"+token+"\n") {
						return []byte("invalid"), nil
					}
					mu.Lock()
					spans[token].ended = true
					mu.Unlock()
					return []byte("allowed"), nil
				case policyTrafficScript:
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/", nil)
					resp, err := httpClient.Do(req)
					if err != nil {
						if mode == "readiness-flap" {
							p, e := kube.CoreV1().Pods(ns).Get(ctx, "server", metav1.GetOptions{})
							if e == nil {
								p.Status.Conditions = nil
								_, _ = kube.CoreV1().Pods(ns).UpdateStatus(ctx, p, metav1.UpdateOptions{})
							}
						}
						if strings.Contains(strings.ToLower(err.Error()), "reset") {
							return []byte("reset"), nil
						}
						return nil, err
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return nil, err
					}
					if string(body) != "oberth-policy-probe" {
						return []byte("invalid"), nil
					}
					return []byte("allowed"), nil
				}
				return nil, fmt.Errorf("unexpected script")
			}}
			timing := policyProbeTiming{2 * time.Second, 100 * time.Millisecond, time.Second, 100 * time.Millisecond, time.Millisecond}
			if mode == "held-response-stalls" {
				timing.total = 180 * time.Millisecond
				timing.traffic = 120 * time.Millisecond
			}
			start := time.Now()
			r := probeNetworkPolicy(t.Context(), Config{}, deps, timing)
			elapsed := time.Since(start)
			namespaces, err := kube.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(namespaces.Items) != 0 || !cleanupUID.Load() {
				t.Errorf("cleanup missing or lacks UID: namespaces=%d UID=%v", len(namespaces.Items), cleanupUID.Load())
			}
			if workers.Load() != 0 {
				t.Errorf("exec workers not joined: %d", workers.Load())
			}
			if elapsed > timing.total+timing.cleanup+time.Second {
				t.Errorf("bound exceeded: %s", elapsed)
			}
			t.Logf("mode=%s no enforcement real HTTP: result=%s elapsed=%s ordinary=%d actual TCP resets=%d remaining namespaces=%d workers=%d evidence=%s remedy=%s", mode, r.Status, elapsed, ordinary.Load(), resets.Load(), len(namespaces.Items), workers.Load(), r.Evidence, r.Remedy)
			if r.Status == "enforced" {
				t.Error("FALSE ENFORCED: held-response pressure rejects new TCP traffic without NetworkPolicy enforcement")
			}
		})
	}
}

func TestPolicyIndependentAlternatingSourcesSeeded(t *testing.T) {
	const trials = 1024
	for _, pattern := range []string{"original windows", "periodic windows"} {
		for _, failure := range []string{"refused", "reset"} {
			t.Run(pattern+"/"+failure, func(t *testing.T) {
				enforced := 0
				synctest.Test(t, func(t *testing.T) {
					for trial := 0; trial < trials; trial++ {
						kube := fake.NewClientset(preflightNode())
						probeNamespaceUIDFixture(kube)
						kube.PrependReactor("create", "pods", readyPolicyModelPod)
						model := &policyTrafficModel{kube: kube, mode: "absent", fault: func(source string, n int) string {
							// Both predicates use only source identity and its own request count.
							// Neither reads policies, selected roles, rounds, seed or probe state.
							if pattern == "original windows" {
								if (policyModelSourceA(source) && n >= 4 && n <= 6) || (!policyModelSourceA(source) && n >= 10 && n <= 12) {
									return failure
								}
							} else {
								offset := 11
								if policyModelSourceA(source) {
									offset = 3
								}
								if n >= offset && (n-offset)%12 < 2 {
									return failure
								}
							}
							return ""
						}}
						random := mathrand.New(mathrand.NewSource(int64(trial + 1)))
						result := probeNetworkPolicyWithRandom(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.runTimed}, policyModelTiming(), random)
						if result.Status == "enforced" {
							enforced++
						}
					}
				})
				t.Logf("pattern=%s failure=%s seeds=1..%d false measured-enforced=%d/%d (%.4f%%)", pattern, failure, trials, enforced, trials, 100*float64(enforced)/trials)
				if pattern == "original windows" && enforced != 0 {
					t.Fatal("original independent-alternation counterexample still passes")
				}
				if enforced > trials/16 {
					t.Fatal("false-pass rate is not far below one half", enforced, trials)
				}
				if pattern == "periodic windows" && enforced == 0 {
					t.Fatal("periodic positive counterexample was not exercised; zero must not be presented as proof of causality")
				}
			})
		}
	}
}

func TestPolicySelectionRandomizationIncludesBothSources(t *testing.T) {
	sequences := map[string]bool{}
	for seed := int64(1); seed <= 1024; seed++ {
		order, err := policyProbeSelections(mathrand.New(mathrand.NewSource(seed)))
		if err != nil || len(order) != policyProbeRounds {
			t.Fatal(order, err)
		}
		key := strings.Join(order, ",")
		if !strings.Contains(key, "client") || !strings.Contains(key, "control") {
			t.Fatal("missing source", order)
		}
		sequences[key] = true
	}
	if len(sequences) != 62 {
		t.Fatal("expected all six-bit sequences containing both sources", len(sequences))
	}
	for _, bit := range []byte{0, 1} {
		if _, err := policyProbeSelections(bytes.NewReader(bytes.Repeat([]byte{bit}, 8*policyProbeRounds))); err == nil {
			t.Fatal("degenerate randomness was not bounded")
		}
	}
}

func TestPolicySeededModelCanCompleteAllRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for seed := int64(1); seed <= 64; seed++ {
			kube := fake.NewClientset(preflightNode())
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "timeout"}
			result := probeNetworkPolicyWithRandom(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.runTimed}, policyModelTiming(), mathrand.New(mathrand.NewSource(seed)))
			if result.Status != "enforced" {
				t.Fatalf("seed %d positive model did not complete: %+v", seed, result)
			}
		}
	})
}
