package installer

import (
	"context"
	"errors"
	"reflect"
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
