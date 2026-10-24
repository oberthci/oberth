package installer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestUpgradeWaitIdleTargetsSelectedDeploymentAndStopsOnFailure(t *testing.T) {
	wantErr := errors.New("queue observation failed")
	cfg := UpgradeConfig{Namespace: "chosen", Timeout: time.Second, WaitIdle: true}
	calls := 0
	deps := Deps{ContextName: "chosen-context", RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		calls++
		want := []string{"--context", "chosen-context", "exec", "-c", "oberth", "-n", "chosen", "deploy/oberth", "--", "oberth", "wait-idle", "--timeout=2h0m0s"}
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

func TestUpgradeWaitTimeoutIndependentAndCancelable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		start := time.Now()
		err := waitForUpgradeIdle(t.Context(), UpgradeConfig{WaitIdle: true, WaitTimeout: 3 * time.Minute, Timeout: time.Minute}, Deps{Output: &output, RunCommand: func(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			if !strings.Contains(strings.Join(args, " "), "--timeout=3m0s") {
				t.Fatal(args)
			}
			<-ctx.Done()
			return nil, errors.New("signal: killed")
		}})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 3*time.Minute || !strings.Contains(err.Error(), "upgrade not attempted") || !strings.Contains(output.String(), "elapsed=2m0s") {
			t.Fatalf("elapsed=%s error=%v output=%s", time.Since(start), err, &output)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := waitForUpgradeIdle(ctx, UpgradeConfig{WaitTimeout: time.Hour}, Deps{RunCommand: func(ctx context.Context, _ []byte, _ string, _ ...string) ([]byte, error) { return nil, ctx.Err() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUpgradeWaitCriticalAndStreamProgress(t *testing.T) {
	var output bytes.Buffer
	err := waitForUpgradeIdle(t.Context(), UpgradeConfig{WaitFor: "critical", WaitTimeout: time.Hour}, Deps{Output: &output, RunCommandStream: func(ctx context.Context, w io.Writer, name string, args ...string) error {
		if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "--wait-for=critical") {
			t.Fatal(name, args)
		}
		_, err := io.WriteString(w, "queued=2 running=1 branch=1 critical=2\n")
		return err
	}})
	if err != nil || !strings.Contains(output.String(), "queued=2 running=1 branch=1 critical=2") {
		t.Fatalf("%v %s", err, &output)
	}
}
