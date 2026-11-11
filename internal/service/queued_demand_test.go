package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type queueDemandReaderStub struct {
	ResourceDeclarationReader
	calls int
}

func (r *queueDemandReaderStub) RunPodDemand(_ context.Context, request WorkflowRequest) (*argoworkflow.PodDemand, error) {
	r.calls++
	return &argoworkflow.PodDemand{CPUMilli: 9900, MemoryBytes: 16 << 30, Pods: 2}, nil
}

func TestQueuedDemandVisibleWithinScopeAndCached(t *testing.T) {
	f := newControlFixture(t)
	cause := "branch CI: park policy unavailable (read promotion policy: context deadline exceeded)"
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "queue-demand", Repository: f.repo, Branch: "feature/demand", SHA: strings.Repeat("a", 40), Actor: "dev", Reason: cause})
	if err != nil {
		t.Fatal(err)
	}
	control := f.api(t)
	reader := &queueDemandReaderStub{}
	control.resourceDeclarations = reader
	for _, actor := range []api.Actor{{Identity: "owner"}, {Identity: "scoped", Scope: []string{"codeberg"}}} {
		for _, view := range []string{""} {
			value, err := control.Runs(t.Context(), actor, api.RunFilter{Ref: queued.Ref, View: view})
			if err != nil {
				t.Fatal(err)
			}
			var run model.Run
			switch rows := value.(type) {
			case []model.Run:
				run = rows[0]
			default:
				t.Fatalf("%T", value)
			}
			want := "needs 9.9 CPU, 16 GiB · next in line"
			if actor.Scoped() {
				want = "needs 9.9 CPU, 16 GiB · in line"
			}
			if run.DisplayReason != want || strings.Contains(run.DisplayReason, "policy") || !strings.Contains(run.Reason, cause) {
				t.Fatalf("run=%+v", run)
			}
		}
		value, err := control.Run(t.Context(), actor, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		detail := value.(RunDetailResponse)
		if detail.Scheduling.Requests == nil || detail.Scheduling.Requests.CPUMilli != 9900 || !strings.Contains(detail.Run.Reason, cause) {
			t.Fatal("detail lost requests or diagnostic", detail)
		}
	}
	if reader.calls != 1 {
		t.Fatalf("polling repeated graph reads: %d", reader.calls)
	}
	if _, err := control.Run(t.Context(), api.Actor{Identity: "hidden", Scope: []string{"hidden"}}, queued.ID); err == nil {
		t.Fatal("hidden run exposed cached demand")
	}
	if reader.calls != 1 {
		t.Fatal("unauthorized source read")
	}
}

func TestQueueMessageHasRequestsAndOneBlocker(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation model.AdmissionObservation
		position    int64
		want        string
	}{
		{name: "CPU pressure", observation: model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "cpu"}, Runs: 6}, want: "host CPU busy"},
		{name: "IO pressure", observation: model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "io"}}, want: "host IO busy"},
		{name: "behind", observation: model.AdmissionObservation{BlockedBy: &model.AdmissionBlocker{Repo: "org/repo", Ref: "main"}}, want: "behind org/repo/main"},
		{name: "head", observation: model.AdmissionObservation{HeadOfQueue: true}, want: "next in line"},
		{name: "position", observation: model.AdmissionObservation{Position: 3}, want: "position 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.observation.State = "waiting"
			tc.observation.Requests = &argoworkflow.PodRequests{CPUMilli: 9900, MemoryBytes: 16 << 30, Pods: 2}
			got := queuedDisplayReason(&model.RunScheduling{State: "waiting", Admission: &tc.observation}, nil)
			if got != "needs 9.9 CPU, 16 GiB · "+tc.want || strings.Count(got, " · ") != 1 {
				t.Fatal(got)
			}
			for _, forbidden := range []string{"capacity", "admission", "declared", "effective"} {
				if strings.Contains(got, forbidden) {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestWorkflowOnlyReservationReportsBoundPodRequests(t *testing.T) {
	engine := &groupEngine{numericCPU: map[string]int64{"feature/requests": 7900}}
	request := WorkflowRequest{Run: model.Run{ID: strings.Repeat("a", 32), SHA: strings.Repeat("b", 40), Ref: "feature/requests"}, Repository: model.Repository{ID: 1}}
	receipt, err := engine.PipelineDemandReceipt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	gate := newAdmissionGate()
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "occupant"})
	defer release()
	demand, err := receipt.WorkflowPodDemand()
	if err != nil {
		t.Fatal(err)
	}
	reservation := pending(t, gate, 2, admissionRequest{RunID: request.Run.ID, Requests: queuePodRequests(demand)})
	defer reservation.cancel()
	snapshot := (&Scheduler{admission: gate}).AdmissionSnapshot(2)
	if snapshot.Requests == nil || snapshot.Requests.CPUMilli != 7900 {
		t.Fatalf("requests lost or retired budget restored: %+v", snapshot)
	}
	snapshot.Requests.CPUMilli = 1
	if (&Scheduler{admission: gate}).AdmissionSnapshot(2).Requests.CPUMilli != 7900 {
		t.Fatal("snapshot mutated reservation")
	}
	// Sizing is diagnostic only; releasing the occupied slot still grants this run.
	release()
	deadline := time.Now().Add(time.Second)
	for !granted(gate, reservation) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !granted(gate, reservation) {
		t.Fatal("request arithmetic blocked workflow admission")
	}
	reservation.release()
}

type queueDemandAdmission struct{}

func (queueDemandAdmission) AdmissionSnapshot(int64) *model.AdmissionObservation {
	return &model.AdmissionObservation{State: "waiting", Requests: &argoworkflow.PodRequests{CPUMilli: 9900, MemoryBytes: 16 << 30, Pods: 2}, Runs: 6, Pressure: &model.PressureDeferral{Resource: "cpu"}, BlockedBy: &model.AdmissionBlocker{RepoID: 999, Repo: "secret", Ref: "private"}}
}

func TestScopedWaitingRequestsUseBoundReceiptWithoutGlobalCounts(t *testing.T) {
	f := newControlFixture(t)
	_, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "waiting-demand", Repository: f.repo, Branch: "feature/waiting", SHA: strings.Repeat("a", 40), Actor: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	control := f.api(t)
	control.admission = queueDemandAdmission{}
	reader := &queueDemandReaderStub{}
	control.resourceDeclarations = reader
	value, err := control.Run(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"codeberg"}}, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Run.DisplayReason != "needs 9.9 CPU, 16 GiB · host CPU busy" || detail.Scheduling.Admission.Runs != 0 || detail.Scheduling.Admission.BlockedBy != nil || reader.calls != 0 {
		t.Fatalf("scoped waiting view=%+v scheduling=%+v reads=%d", detail.Run, detail.Scheduling, reader.calls)
	}
}
