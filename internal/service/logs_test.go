package service_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogSelectorsOptionsAndMetadataPreserved(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	zero := 0
	no := false
	offset := int64(0)
	q := logRequest{viewQuery: viewQuery{ID: runID}, Step: "build", StepSet: true, Context: &zero, Offset: &zero, Limit: &zero, Tail: &no, ByteOffset: &offset, Selector: stepmodel.LogSelector{NodeID: "n", PodName: "pod", Container: "authored"}, Containers: true, Raw: true}
	f.logs.ReadFunc = func(_ context.Context, r stateport.Run, got apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		checkLogRequest(t, r, got, q)
		return apicontract.LogResult{Content: "output", Meta: logcontract.Meta{Follow: true, HasMore: true, NextByteOffset: 23}}, nil
	}
	out, err := f.s.RunLogs(t.Context(), caller(), q)
	if err != nil || out.ID != runID || !reflect.DeepEqual(out.Meta, logcontract.Meta{Follow: true, HasMore: true, NextByteOffset: 23}) {
		t.Fatalf("logs %+v %v", out, err)
	}
}
func checkLogRequest(t *testing.T, r stateport.Run, got apicontract.RunLogsArgs, q logRequest) {
	t.Helper()
	if r.ID != runID || got.Context == nil || *got.Context != 0 || got.Offset == nil || got.Limit == nil {
		t.Fatalf("log options %+v", got)
	}
	checkLogSelection(t, got, q)
}
func checkLogSelection(t *testing.T, got apicontract.RunLogsArgs, q logRequest) {
	t.Helper()
	if got.Tail == nil || *got.Tail || got.ByteOffset == nil || got.Step != q.Step || got.NodeID != q.Selector.NodeID || got.Container != q.Selector.Container || !got.Containers || !got.Raw {
		t.Fatalf("log selectors %+v", got)
	}
}
func TestLogSelectionByCommitAndRef(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	for _, q := range []viewQuery{{SHA: head[:8]}, {Ref: "refs/heads/topic"}, {ID: runID[:8]}} {
		out, err := f.s.Logs(t.Context(), caller(), logRequest{viewQuery: q})
		if err != nil || out.ID != runID {
			t.Fatalf("selector %+v %v", out, err)
		}
	}
}
func TestLogsAuthorizeOnce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	_, err := f.s.Logs(t.Context(), caller(), logRequest{viewQuery: viewQuery{SHA: head}})
	if err != nil || f.store.Calls()["Current"] != 1 {
		t.Fatal("nested authorization", err)
	}
}

func TestScanRequiresCredentialIdentity(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	_, err := f.s.RunLogs(t.Context(), caller(), logRequest{viewQuery: viewQuery{ID: runID}, Pattern: "match"})
	if err == nil {
		t.Fatal("scan without private credential ID")
	}
}

// Covers: REQ-C2-008
func TestBoundedScopedReadsContract(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	entered := make(chan string, 3)
	release := make(chan struct{})
	f.logs.ReadFunc = func(ctx context.Context, _ stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		entered <- q.Step
		select {
		case <-release:
			return apicontract.LogResult{}, nil
		case <-ctx.Done():
			return apicontract.LogResult{}, ctx.Err()
		}
	}
	done := make(chan error, 3)
	start := func(key string) {
		go func() {
			_, err := f.s.RunLogs(t.Context(), caller(), logRequest{viewQuery: viewQuery{ID: runID}, Pattern: "match", CredentialID: key})
			done <- err
		}()
	}
	start("first")
	if receive(t, entered) != "first" {
		t.Fatal("first scan")
	}
	start("first")
	start("second")
	if receive(t, entered) != "second" {
		t.Fatal("same credential took both slots")
	}
	close(release)
	for range 3 {
		if err := receive(t, done); err != nil {
			t.Fatal(err)
		}
	}
}
func TestScanSlotsReleasedOnFailure(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	failure := errors.New("read failed")
	f.logs.ReadFunc = func(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		return apicontract.LogResult{}, failure
	}
	q := logRequest{viewQuery: viewQuery{ID: runID}, Pattern: "match", CredentialID: "private-id"}
	for range 3 {
		_, err := f.s.RunLogs(t.Context(), caller(), q)
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
}
func TestWaitingScanCancelsWithoutHoldingGlobalSlot(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.logs.ReadFunc = func(ctx context.Context, _ stateport.Run, _ apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		close(entered)
		select {
		case <-release:
			return apicontract.LogResult{}, nil
		case <-ctx.Done():
			return apicontract.LogResult{}, ctx.Err()
		}
	}
	q := logRequest{viewQuery: viewQuery{ID: runID}, Pattern: "match", CredentialID: "private-id"}
	done := make(chan error, 1)
	go func() { _, err := f.s.RunLogs(t.Context(), caller(), q); done <- err }()
	receive(t, entered)
	ctx, cancel := context.WithCancel(t.Context())
	call, err := f.s.PrepareLogs(ctx, caller(), q)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = call(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalScanBoundAndCancelableThird(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunRunning)
		entered := make(chan string, 3)
		release := make(chan struct{})
		done := make(chan error, 3)
		f.logs.ReadFunc = func(_ context.Context, _ stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
			entered <- q.Step
			<-release
			return apicontract.LogResult{}, nil
		}
		launch := func(ctx context.Context, key string) {
			go func() {
				_, err := f.s.RunLogs(ctx, caller(), logRequest{viewQuery: viewQuery{ID: runID}, Pattern: "x", CredentialID: key})
				done <- err
			}()
		}
		launch(t.Context(), "a")
		launch(t.Context(), "b")
		synctest.Wait()
		if len(entered) != 2 {
			t.Fatal("two scans not admitted")
		}
		third, cancel := context.WithCancel(t.Context())
		launch(third, "c")
		synctest.Wait()
		if len(entered) != 2 {
			t.Fatal("third scan exceeded global capacity")
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		synctest.Wait()
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	})
}
