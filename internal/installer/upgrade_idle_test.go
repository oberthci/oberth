package installer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUpgradeWaitIdleTargetsSelectedDeploymentAndStopsOnFailure(t *testing.T) {
	wantErr := errors.New("queue observation failed")
	cfg := UpgradeConfig{Namespace: "chosen", Timeout: time.Second, WaitIdle: true}
	calls := 0
	deps := Deps{ContextName: "chosen-context", RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		calls++
		want := []string{"--context", "chosen-context", "exec", "-c", "oberth", "-n", "chosen", "deploy/oberth", "--", "oberth", "wait-idle", "--timeout=1s"}
		if name != "kubectl" || !reflect.DeepEqual(args, want) || len(input) != 0 {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("idle wait has no deadline")
		}
		return nil, wantErr
	}}
	if err := waitForUpgradeIdle(t.Context(), cfg, deps); !errors.Is(err, wantErr) {
		t.Fatalf("lost refusal: %v", err)
	}
	if calls != 1 {
		t.Fatalf("blindly retried upgrade preparation: %d", calls)
	}
}

func TestUpgradeWaitIdlePreservesContextCause(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			childErr := errors.New("signal: killed")
			deps := Deps{RunCommand: func(ctx context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
				<-ctx.Done()
				return nil, childErr
			}}
			err := waitForUpgradeIdle(ctx, UpgradeConfig{Timeout: time.Millisecond}, deps)
			cause := context.DeadlineExceeded
			if canceled {
				cause = context.Canceled
			}
			if !errors.Is(err, cause) || !errors.Is(err, childErr) || !strings.Contains(err.Error(), "upgrade not attempted") {
				t.Fatalf("lost wait failure cause: %v", err)
			}
		})
	}
}
