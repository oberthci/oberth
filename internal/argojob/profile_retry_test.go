package argojob

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestProfileReadFailureRetriesBeforePreparingSubmission(t *testing.T) {
	for _, failure := range []error{errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline"), context.DeadlineExceeded, errors.New("API temporarily unavailable")} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pod, cm := nonrootVerifierObjects()
				client := fake.NewClientset(pod, cm)
				reads := 0
				client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
					reads++
					if reads <= 3 {
						return true, nil, failure
					}
					return false, nil, nil
				})
				config := testConfig()
				config.NonrootProfile = argoworkflow.NonrootStaticProfile
				controller, err := NewController(&staticWorkflowClient{}, client, config)
				if err != nil {
					t.Fatal(err)
				}
				request, err := controller.PrepareNonroot(t.Context(), testRequest(periapsis.TriggerCI, nonrootTestDocument))
				if err != nil || request.nonrootProof == nil || reads != 4 {
					t.Fatalf("profile read failure escaped instead of recovering: reads=%d proof=%v error=%v", reads, request.nonrootProof != nil, err)
				}
			})
		})
	}
}

func TestProfileOutageBlocksAdmissionAndCancellationDoesNotAuthorize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pod, cm := nonrootVerifierObjects()
		client := fake.NewClientset(pod, cm)
		var unavailable atomic.Bool
		unavailable.Store(true)
		client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
			if unavailable.Load() {
				return true, nil, errors.New("client rate limiter: synthetic-private-detail")
			}
			return false, nil, nil
		})
		config := testConfig()
		config.NonrootProfile = argoworkflow.NonrootStaticProfile
		controller, err := NewController(&staticWorkflowClient{}, client, config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		prepared := make(chan error, 1)
		go func() {
			_, err := controller.PrepareNonroot(ctx, testRequest(periapsis.TriggerCI, nonrootTestDocument))
			prepared <- err
		}()
		synctest.Wait()
		state := controller.ControllerProfileStatus()
		if state.State != "blocked" || state.ErrorClass != "throttled" || state.ReadFailuresTotal != 1 || state.RetryAt.IsZero() {
			t.Fatalf("unobservable profile outage: %#v", state)
		}
		admitted := make(chan error, 1)
		go func() { admitted <- controller.AwaitAdmission(t.Context()) }()
		synctest.Wait()
		select {
		case err := <-admitted:
			t.Fatalf("admission escaped outage: %v", err)
		default:
		}
		cancel()
		if err := <-prepared; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller: %v", err)
		}
		if controller.ControllerProfileStatus().State != "blocked" {
			t.Fatal("cancellation cleared the gate")
		}
		unavailable.Store(false)
		if err := <-admitted; err != nil {
			t.Fatal(err)
		}
		state = controller.ControllerProfileStatus()
		if state.State != "verified" || state.ErrorClass != "" || state.ReadFailuresTotal != 1 || !state.RetryAt.IsZero() {
			t.Fatalf("admission did not recover cleanly: %#v", state)
		}
	})
}

func TestProfileRetryBackoffIsCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pod, cm := nonrootVerifierObjects()
		client := fake.NewClientset(pod, cm)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var attempts []time.Time
		client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
			attempts = append(attempts, time.Now())
			if len(attempts) == 9 {
				cancel()
			}
			return true, nil, context.DeadlineExceeded
		})
		config := testConfig()
		config.NonrootProfile = argoworkflow.NonrootStaticProfile
		controller, err := NewController(&staticWorkflowClient{}, client, config)
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.PrepareNonroot(ctx, testRequest(periapsis.TriggerCI, nonrootTestDocument))
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		for i, want := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
			if got := attempts[i+1].Sub(attempts[i]); got != want {
				t.Fatalf("retry %d: %v, want %v", i, got, want)
			}
		}
	})
}
