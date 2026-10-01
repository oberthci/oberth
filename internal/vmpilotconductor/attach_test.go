package vmpilotconductor

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/vmnodebridge"
	"github.com/oberthci/oberth/internal/vmpilotfixture"
	"github.com/oberthci/oberth/internal/vmrunner"
)

func TestClaimedAttachFailureStaysSpentAndRequiresCleanup(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	pod := f.pod(t, "first")
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("a", 64)
	f.savePod(t, pod)
	_, err := f.b.RunBoundStream(f.ctx, f.plan, new(vmnodebridge.Client), new(vmpilotfixture.Fixture),
		vmrunner.PilotEndpoint{}, func(context.Context, vmrunner.PilotRestartRequest) (vmrunner.PilotRestartResponse, error) {
			t.Fatal("failed channel invoked restart")
			return vmrunner.PilotRestartResponse{}, nil
		})
	if err == nil {
		t.Fatal("unavailable node channel accepted")
	}
	value, err := f.s.PilotExecution(f.ctx, f.plan.Spec.RunID)
	must(t, err)
	if value.Attach == nil || !value.Attach.BoundAt.IsZero() || value.Failure != "runtime" || !value.Cleaning || value.Receipt != nil {
		t.Fatalf("ambiguous attach was not spent and failed: %#v", value)
	}
	if _, granted, err := f.s.ClaimConductorAttach(f.ctx, f.plan.Spec.RunID, *value.Attempt); err != nil || granted {
		t.Fatalf("failed attach regranted: granted=%v err=%v", granted, err)
	}
}
