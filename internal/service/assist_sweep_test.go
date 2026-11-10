package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type assistSweepStore struct {
	SchedulerStore
	activity map[string]model.AssistPodActivity
	appended []model.RunAssistRequest
	runs     map[string]model.Run
}

func (store *assistSweepStore) Run(_ context.Context, id string) (model.Run, error) {
	if run, ok := store.runs[id]; ok {
		return run, nil
	}
	return model.Run{}, errors.New("run not found")
}

func (store *assistSweepStore) AssistPodActivity(context.Context) (map[string]model.AssistPodActivity, error) {
	return store.activity, nil
}

func (store *assistSweepStore) AppendRunAssist(_ context.Context, request model.RunAssistRequest) (model.Run, error) {
	store.appended = append(store.appended, request)
	return model.Run{ID: request.RunID}, nil
}

type assistSweepJobs struct {
	JobController
	activity map[string]model.AssistPodActivity
	recheck  model.AssistActivityRecheck
	abandons []model.AssistAbandon
}

func (jobs *assistSweepJobs) EnforceAssistHolds(_ context.Context, activity map[string]model.AssistPodActivity, recheck model.AssistActivityRecheck, _ time.Time) ([]model.AssistAbandon, error) {
	jobs.activity, jobs.recheck = activity, recheck
	return jobs.abandons, nil
}

// TestSweepAssistHoldsRecordsEachAbandon proves the sweep hands the store's
// per-Pod activity to the engine and appends a server abandon event, with
// reason and times, to the run of every hold it abandoned.
func TestSweepAssistHoldsRecordsEachAbandon(t *testing.T) {
	exit := 1
	held := time.Date(2026, 11, 9, 20, 14, 24, 0, time.UTC)
	abandon := model.AssistAbandon{RunID: "1886f0010bd88cf198044015e23e463a", Pod: "wf-test-1", Step: "test", Reason: model.AssistAbandonTTL,
		HeldSince: held, AbandonedAt: held.Add(20 * time.Minute), FailedExitCode: &exit}
	store := &assistSweepStore{activity: map[string]model.AssistPodActivity{"wf-lint-1": {Released: true}}}
	jobs := &assistSweepJobs{abandons: []model.AssistAbandon{abandon}}
	scheduler := &Scheduler{store: store, jobs: jobs, signals: NewSignals()}
	scheduler.sweepAssistHolds(context.Background())
	if !jobs.activity["wf-lint-1"].Released {
		t.Fatalf("engine did not get the store's activity: %v", jobs.activity)
	}
	if len(store.appended) != 1 {
		t.Fatalf("appended = %+v", store.appended)
	}
	event := store.appended[0].Event
	if store.appended[0].RunID != abandon.RunID || event.Kind != model.RunAssistAbandon || event.Actor != "oberth" || event.Step != "test" || event.Pod != "wf-test-1" ||
		event.AbandonReason != model.AssistAbandonTTL || event.HeldSince == nil || !event.HeldSince.Equal(held) || !event.At.Equal(abandon.AbandonedAt) || event.FailedExitCode == nil || *event.FailedExitCode != 1 {
		t.Fatalf("event = %+v", event)
	}
}

// TestAbandonedHoldReasonLineIsNotADeadline is the reason line of RCA H4: the
// kubelet or the sweep ended a held retry, and the run error carries the
// held Pod's inflated deadline. The line names the step's first attempt and
// the hold instead, without "· deadline".
func TestAbandonedHoldReasonLineIsNotADeadline(t *testing.T) {
	exit := 1
	held := time.Date(2026, 11, 9, 20, 14, 24, 0, time.UTC)
	record := model.RunAssistRecord{Events: []model.RunAssistEvent{model.AssistAbandon{Pod: "wf-test-1", Step: "test", Reason: model.AssistAbandonCap,
		HeldSince: held, AbandonedAt: held.Add(90 * time.Second), FailedExitCode: &exit}.Event()}}
	run := model.Run{ID: "1886f0010bd88cf198044015e23e463a", Status: model.RunFailed, FailedStage: "ci", FailedStep: "test", AssistRequested: true, Assist: &record,
		Error: model.ExecutionDeadlineMessage + " Step ci/test: deadline limit 2733s; observed runtime 46m0s; sampled memory peak unknown (lower bound). Inspect oberth run resources --run 1886f0010bd88cf198044015e23e463a --include-failed; change deadlines in git."}
	want := "step test failed at attempt 0 (exit 1); hold abandoned unserviced after 1m30s (cap)"
	if got := presentationRun(run).DisplayReason; got != want {
		t.Fatalf("display reason = %q, want %q", got, want)
	}
	// Another step failing in the same run keeps its own wording.
	run.FailedStep = "lint"
	if got := presentationRun(run).DisplayReason; !strings.HasPrefix(got, "lint failed · deadline") || !strings.HasSuffix(got, want) {
		t.Fatalf("other step display reason = %q", got)
	}
}

// TestSweepAssistHoldsRecheckReadsTheVictimsRun: the engine's per-victim
// recheck reads the run's current assist record, so a continue written after
// the snapshot releases the Pod; a run that cannot be read is an error, which
// spares the victim.
func TestSweepAssistHoldsRecheckReadsTheVictimsRun(t *testing.T) {
	continued := model.RunAssistRecord{Events: []model.RunAssistEvent{{At: time.Now(), Actor: "uplink-a", Step: "test", Pod: "wf-test-1", Kind: model.RunAssistContinue}}}
	store := &assistSweepStore{activity: map[string]model.AssistPodActivity{}, runs: map[string]model.Run{"run-a": {ID: "run-a", Assist: &continued}}}
	jobs := &assistSweepJobs{}
	scheduler := &Scheduler{store: store, jobs: jobs, signals: NewSignals()}
	scheduler.sweepAssistHolds(context.Background())
	if jobs.recheck == nil {
		t.Fatal("the sweep gave the engine no per-victim recheck")
	}
	if fresh, err := jobs.recheck(context.Background(), "run-a", "wf-test-1"); err != nil || !fresh.Released {
		t.Fatalf("recheck = %+v %v, want the continue written after the snapshot", fresh, err)
	}
	if _, err := jobs.recheck(context.Background(), "run-missing", "wf-test-1"); err == nil {
		t.Fatal("an unreadable run did not fail the recheck")
	}
}
