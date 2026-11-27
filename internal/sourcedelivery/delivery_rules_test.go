package sourcedelivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestSourceLoadRequiresTheExactSealedAdmissionAndDescriptor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*sourcedelivery.Run, *sourcecontract.Descriptor)
		want   bool
	}{
		{"exact", func(*sourcedelivery.Run, *sourcecontract.Descriptor) {}, true},
		{"another-admission", func(r *sourcedelivery.Run, _ *sourcecontract.Descriptor) { r.Admission.RunID = "another-run" }, false},
		{"unsealed-admission", func(r *sourcedelivery.Run, _ *sourcecontract.Descriptor) { r.Admission.Digest = "" }, false},
		{"undeclared-input", func(_ *sourcedelivery.Run, d *sourcecontract.Descriptor) {
			*d = sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/library", Commit: strings.Repeat("b", 40)}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := deliveryRulesConfig()
			run, err := config.Run(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
			tc.change(&run, &descriptor)
			config.Run = func(context.Context, string) (sourcedelivery.Run, error) { return run, nil }
			staged := 0
			stage := config.Stage
			config.Stage = func(ctx context.Context, b sourcecontract.Binding) (sourcedelivery.Staged, error) {
				staged++
				return stage(ctx, b)
			}
			response := requestRulesSource(t, sourcedelivery.New(config), descriptor)
			if tc.want {
				if response.Code != http.StatusOK || staged != 1 || response.Header().Get(sourcedelivery.ReceiptTrailer) == "" {
					t.Fatalf("exact source refused: status %d, stages %d", response.Code, staged)
				}
			} else if response.Code != http.StatusForbidden || staged != 0 || response.Body.String() != "{\"version\":1,\"code\":\"forbidden\"}\n" {
				t.Fatalf("invalid selection reached source: status %d, stages %d, body %s", response.Code, staged, response.Body.String())
			}
		})
	}
}

func TestSourceCloseFailureWithholdsCompletionReceipt(t *testing.T) {
	t.Parallel()
	config := deliveryRulesConfig()
	closed, failed := 0, 0
	config.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		return sourcedelivery.Staged{Body: &failedSourceClose{Reader: strings.NewReader("source"), closed: &closed}, Bytes: 6}, nil
	}
	config.Metric = func(summary sourcedelivery.Summary) {
		if summary.Code != "unavailable" {
			t.Errorf("unexpected metric %q", summary.Code)
		}
		failed++
	}
	response := requestRulesSource(t, sourcedelivery.New(config), sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	if response.Body.String() != "source" || response.Header().Get(sourcedelivery.ReceiptTrailer) != "" || closed != 1 || failed != 1 {
		t.Fatalf("completion after close failure: headers %v, close %d, failures %d", response.Header(), closed, failed)
	}
}

type failedSourceClose struct {
	io.Reader
	closed *int
}

func (r *failedSourceClose) Close() error {
	*r.closed++
	return errors.New("source close failed")
}

func TestSourceTransferSlotsWaitForAThirdAndAreReleased(t *testing.T) {
	t.Parallel()
	config := deliveryRulesConfig()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var stages atomic.Int32
	config.Stage = func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		if stages.Add(1) <= 2 {
			entered <- struct{}{}
			<-release
		}
		return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
	}
	server := sourcedelivery.New(config)
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	var workers sync.WaitGroup
	responses := make(chan int, 2)
	for range 2 {
		workers.Go(func() {
			responses <- requestRulesSource(t, server, descriptor).Code
		})
	}
	var unblock sync.Once
	join := func() { unblock.Do(func() { close(release) }); workers.Wait() }
	t.Cleanup(join)
	awaitSourceStages(t, entered, responses)
	third := make(chan *httptest.ResponseRecorder, 1)
	workers.Go(func() { third <- requestRulesSource(t, server, descriptor) })
	select {
	case response := <-third:
		t.Fatalf("third transfer returned before a slot was released: %d", response.Code)
	case <-time.After(20 * time.Millisecond):
	}
	if stages.Load() != 2 {
		t.Fatal("transfer capacity exceeded", stages.Load())
	}
	join()
	for range 2 {
		if code := <-responses; code != http.StatusOK {
			t.Fatalf("active transfer failed: %d", code)
		}
	}
	if response := <-third; response.Code != http.StatusOK || stages.Load() != 3 {
		t.Fatalf("released slots unavailable: status %d, stages %d", response.Code, stages.Load())
	}
}

func awaitSourceStages(t *testing.T, entered <-chan struct{}, responses <-chan int) {
	t.Helper()
	for range 2 {
		select {
		case <-entered:
		case code := <-responses:
			t.Fatalf("active transfer returned before release: %d", code)
		}
	}
}

func TestTokenReviewErrorCannotReachThePodReader(t *testing.T) {
	t.Parallel()
	podReads := 0
	auth := sourcedelivery.KubernetesAuth{
		Review: func(context.Context, *authv1.TokenReview) (*authv1.TokenReview, error) {
			return &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, Error: "review failed", User: authv1.UserInfo{Username: "system:serviceaccount:runs:pipeline", Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"pod"}, "authentication.kubernetes.io/pod-uid": {"uid"}}}}}, nil
		},
		Pod: func(context.Context, string, string) (*corev1.Pod, error) { podReads++; return nil, nil },
	}
	_, err := auth.Authenticate(t.Context(), "bound-token")
	var failure *sourcedelivery.Failure
	if !errors.As(err, &failure) || failure.Code != "unauthenticated" || podReads != 0 {
		t.Fatalf("TokenReview error established identity: %v, Pod reads %d", err, podReads)
	}
}

func deliveryRulesConfig() sourcedelivery.Config {
	descriptor := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	return sourcedelivery.Config{
		SourceClientBurst: 9, Now: func() time.Time { return time.Unix(1, 0) },
		Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
			return sourcedelivery.Caller{Namespace: "runs", Workflow: "workflow", UID: "workflow-uid"}, nil
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) {
			return sourcedelivery.Run{Namespace: "runs", Workflow: "workflow", UID: "workflow-uid", Admission: admissioncontract.PreparedView{RunID: "run", Digest: strings.Repeat("a", 64), SourceBindings: []sourcecontract.Binding{{Descriptor: descriptor}}}}, nil
		},
		Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
			return sourcedelivery.Staged{Body: io.NopCloser(strings.NewReader("source")), Bytes: 6}, nil
		},
	}
}

func requestRulesSource(t *testing.T, server *sourcedelivery.Server, descriptor sourcecontract.Descriptor) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(sourcedelivery.LoadRequest{Locator: "run", Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer bound-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}
