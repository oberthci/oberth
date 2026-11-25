package runlog

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestInfrastructureReceiptSurvivesConcurrentCaptureAndReopen(t *testing.T) {
	s := newStore(t, 4096)
	r := InfrastructureReceipt{RunID: "run", WorkflowUID: "workflow-uid", NodeID: "one", Step: "build", PodUID: "pod-one", Kind: "secret"}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.RecordInfrastructure(t.Context(), r); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Go(func() { capture(t, s, native("one"), "author output") })
	wg.Wait()
	reopened, err := Open(t.Context(), s.directory, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Infrastructure(t.Context(), "run")
	if err != nil || !reflect.DeepEqual(got, []InfrastructureReceipt{r}) {
		t.Fatal("receipt lost or duplicated", got, err)
	}
	got[0].Kind = "source"
	fresh, err := reopened.Infrastructure(t.Context(), "run")
	if err != nil || fresh[0] != r {
		t.Fatal("caller mutated saved receipt", fresh, err)
	}
	other, err := reopened.Infrastructure(t.Context(), "another-run")
	if err != nil || len(other) != 0 {
		t.Fatal("cross-run receipt", other, err)
	}
}

func TestInfrastructureSuccessSupersedesFailureAcrossReopenAndWriteOrder(t *testing.T) {
	s := newStore(t, 4096)
	r := InfrastructureReceipt{RunID: "run", WorkflowUID: "workflow", NodeID: "one", Step: "build", PodUID: "pod", Kind: "secret", StartedAt: time.Unix(100, 0), FinishedAt: time.Unix(101, 0)}
	if err := s.RecordInfrastructure(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	success := r
	success.FinishedAt, success.Delivered = time.Unix(102, 0), true
	if err := s.RecordInfrastructure(t.Context(), success); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), s.directory, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	// Simulate a failure writer which reached the filesystem after the success.
	if err = reopened.RecordInfrastructure(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Infrastructure(t.Context(), r.RunID); err != nil || len(got) != 0 {
		t.Fatal("success resurrected failure", got, err)
	}
	r.FinishedAt = time.Unix(103, 0)
	if err = reopened.RecordInfrastructure(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Infrastructure(t.Context(), r.RunID); err != nil || len(got) != 1 {
		t.Fatal("new failure disappeared", got, err)
	}
}

func TestInfrastructureReceiptRejectsInvalidIdentity(t *testing.T) {
	s := newStore(t, 4096)
	r := InfrastructureReceipt{RunID: "run", WorkflowUID: "workflow-uid", NodeID: "one", PodUID: "pod-one", Kind: "secret"}
	for _, change := range []func(*InfrastructureReceipt){
		func(r *InfrastructureReceipt) { r.RunID = "../run" },
		func(r *InfrastructureReceipt) { r.WorkflowUID = "" },
		func(r *InfrastructureReceipt) { r.NodeID = "" },
		func(r *InfrastructureReceipt) { r.PodUID = "" },
		func(r *InfrastructureReceipt) { r.PodRestarts = -1 },
		func(r *InfrastructureReceipt) { r.Kind = "author-text" },
	} {
		bad := r
		change(&bad)
		if err := s.RecordInfrastructure(t.Context(), bad); err == nil {
			t.Fatal("invalid receipt accepted", bad)
		}
	}
	got, err := s.Infrastructure(t.Context(), "run")
	if err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
