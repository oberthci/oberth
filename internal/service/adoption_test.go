package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type adoptingJobs struct {
	recoveryJobs
	mu       sync.Mutex
	found    bool
	adoptErr error
	adopted  []string
	created  int
	resumed  []runprogress.Event
	retained int64
}

func (jobs *adoptingJobs) Adopt(_ context.Context, run model.Run) (periapsis.Size, bool, error) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.adopted = append(jobs.adopted, run.ID)
	return periapsis.L, jobs.found, jobs.adoptErr
}

func (jobs *adoptingJobs) CreateCI(context.Context, JobRequest) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.created++
	return nil
}

func (jobs *adoptingJobs) WaitResumed(ctx context.Context, name string, output io.Writer, events []runprogress.Event, retained int64) (JobResult, error) {
	jobs.mu.Lock()
	jobs.resumed, jobs.retained = events, retained
	jobs.mu.Unlock()
	return jobs.Wait(ctx, name, output)
}

func waitAdoptedStatus(t *testing.T, database *store.Store, id string, status model.RunStatus) model.Run {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := database.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == status {
			return run
		}
		select {
		case <-deadline:
			t.Fatalf("run = %#v, want %s", run, status)
		case <-ticker.C:
		}
	}
}

func TestStartupAdoptsBranchWorkflowAndFinalizesSameRun(t *testing.T) {
	for _, finishedWhileDown := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "finished-while-down"}[finishedWhileDown], func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			previous := fixture.enqueueAndClaim(t, "feature/adopt", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			replacement, err := fixture.store.RequeueStrandedRun(context.Background(), previous.ID)
			if err != nil {
				t.Fatal(err)
			}
			run, err := fixture.store.ClaimNextRun(context.Background())
			if err != nil || run.ID != replacement.ID || run.AdmissionSequence != previous.AdmissionSequence || run.QueueSequence == previous.QueueSequence {
				t.Fatalf("re-fired fixture lost its admission order: previous=%#v run=%#v err=%v", previous, run, err)
			}
			if err := fixture.store.SetRunConcurrencyGroup(context.Background(), run.ID, "build"); err != nil {
				t.Fatal(err)
			}
			fixture.setJobName(t, run.ID, "oberth-adopt")
			if _, err := fixture.store.MarkRunSubmitted(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{
				terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}),
				waitResult: JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
			}}
			if finishedWhileDown {
				close(jobs.waitBlocks)
			}
			scheduler := fixture.scheduler(t, jobs)
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("[test/unit] before restart\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			event := runprogress.Event{Version: runprogress.Version, Burn: "test", Step: "unit", Status: runprogress.StepPassed, StartedAt: &now, FinishedAt: &now}
			if err := scheduler.logs.AppendStepProgress(run.ID, event); err != nil {
				t.Fatal(err)
			}
			// Reopen as a new owner, exercising the durable recovery transition.
			if err := fixture.store.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.store, err = store.Open(context.Background(), filepath.Join(fixture.root, "oberth.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fixture.store.Close() })
			scheduler = fixture.scheduler(t, jobs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scheduler.Run(ctx) }()
			select {
			case <-jobs.waitEntered:
			case err := <-done:
				t.Fatalf("scheduler stopped: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("adopted Wait not entered")
			}
			if !finishedWhileDown {
				current, err := fixture.store.Run(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status != model.RunRunning || current.SupersededBy != "" || current.QueueSequence != run.QueueSequence || current.AdmissionSequence != run.AdmissionSequence {
					t.Fatalf("adopted run = %#v", current)
				}
				scheduler.admission.mu.Lock()
				used, runs, groups := scheduler.admission.used, scheduler.admission.runs, len(scheduler.admission.groups)
				order := scheduler.admission.reservations[run.QueueSequence].order
				scheduler.admission.mu.Unlock()
				if order != run.AdmissionSequence {
					t.Fatalf("adopted admission order = %d, want persisted %d", order, run.AdmissionSequence)
				}
				if used != scheduler.admission.weights.of(periapsis.L, scheduler.admission.capacity) || runs != 1 || groups != 1 {
					t.Fatalf("admission used/runs/groups = %d/%d/%d", used, runs, groups)
				}
				close(jobs.waitBlocks)
			}
			current := waitAdoptedStatus(t, fixture.store, run.ID, model.RunPassed)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if current.SupersededBy != "" || current.JobName != "oberth-adopt" || current.QueueSequence != run.QueueSequence || current.AdmissionSequence != run.AdmissionSequence {
				t.Fatalf("finalized run = %#v", current)
			}
			if counts, err := fixture.store.CountRuns(context.Background()); err != nil || counts.Total != 2 {
				t.Fatalf("adoption enqueued another run: counts=%#v err=%v", counts, err)
			}
			if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
				t.Fatalf("cancellations = %#v, %v", pending, err)
			}
			steps, err := fixture.store.StepResults(context.Background(), run.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("steps = %#v, %v", steps, err)
			}
			if jobs.created != 0 || len(jobs.deleted) != 0 || len(jobs.adopted) != 1 || jobs.retained != int64(len("[test/unit] before restart\n")) || len(jobs.resumed) != 1 {
				t.Fatalf("adoption did not preserve intent/logs: %#v", jobs)
			}
			scheduler.admission.mu.Lock()
			used, runs := scheduler.admission.used, scheduler.admission.runs
			scheduler.admission.mu.Unlock()
			if used != 0 || runs != 0 {
				t.Fatalf("admission leaked: %d/%d", used, runs)
			}
		})
	}
}

func TestAdoptionMissingWorkflowRequeuesButReadFailureDoesNot(t *testing.T) {
	for _, readErr := range []error{nil, errors.New("API unavailable")} {
		fixture := newRecoveryFixture(t)
		run := fixture.enqueueAndClaim(t, "feature/missing", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		fixture.setJobName(t, run.ID, "oberth-missing")
		jobs := &adoptingJobs{adoptErr: readErr, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
		scheduler := fixture.scheduler(t, jobs)
		err := scheduler.reconcileStrandedRunsWithGate(context.Background(), scheduler.requireMutation, func(model.Run, periapsis.Size) error { return errors.New("absent Workflow adopted") })
		current, lookupErr := fixture.store.Run(context.Background(), run.ID)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if readErr == nil {
			if err != nil || current.Status != model.RunInterrupted || current.SupersededBy == "" {
				t.Fatalf("missing recovery: %#v, %v", current, err)
			}
		} else if err == nil || current.Status != model.RunRunning || current.SupersededBy != "" {
			t.Fatalf("failed observation mutated run: %#v, %v", current, err)
		}
	}
}

func TestAdoptedWorkflowSurvivesShutdownWithWorkspace(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/shutdown", "cccccccccccccccccccccccccccccccccccccccc")
	fixture.setJobName(t, run.ID, "oberth-shutdown")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
	scheduler := fixture.scheduler(t, jobs)
	source := filepath.Join(fixture.root, "work", run.ID, "src")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("scheduler stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Wait not entered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunRunning || current.JobName != "oberth-shutdown" || current.SupersededBy != "" || len(jobs.deleted) != 0 {
		t.Fatalf("shutdown disrupted Workflow: %#v; deleted %v", current, jobs.deleted)
	}
	if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("shutdown cancellations: %#v, %v", pending, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("active workspace removed: %v", err)
	}
}

func TestAdoptedAdmissionIsIdempotentAndHoldsItsGroup(t *testing.T) {
	gate := newWeightedAdmissionForJobs(1)
	run := model.Run{ID: "active", QueueSequence: 11, AdmissionSequence: 2, RepoID: 1, ConcurrencyGroup: "build"}
	first, err := gate.adopt(run, periapsis.L)
	if err != nil {
		t.Fatal(err)
	}
	if first.sequence != run.QueueSequence || first.order != run.AdmissionSequence {
		t.Fatalf("adoption changed admission identity: key=%d order=%d", first.sequence, first.order)
	}
	again, err := gate.adopt(run, periapsis.L)
	if err != nil || again != first {
		t.Fatalf("second adoption = %p, %v", again, err)
	}
	other, err := gate.adopt(model.Run{ID: "also-active", QueueSequence: 2, RepoID: 2}, periapsis.L)
	if err != nil {
		t.Fatal(err)
	}
	if gate.used != 2*gate.weights.of(periapsis.L, gate.capacity) || gate.runs != 2 {
		t.Fatalf("over-capacity adoption = %d/%d", gate.used, gate.runs)
	}
	waiting := gate.reserveRun(model.Run{ID: "next", QueueSequence: 3, RepoID: 1, ConcurrencyGroup: "build"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	granted := make(chan func(), 1)
	go func() {
		release, err := waiting.acquireRequest(ctx, admissionRequest{RunID: "next", RepoID: 1, Group: "build", Size: periapsis.S})
		if err == nil {
			granted <- release
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		gate.mu.Lock()
		prepared := waiting.prepared
		gate.mu.Unlock()
		if prepared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter was not prepared")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-granted:
		t.Fatal("new work bypassed adopted weight/group")
	default:
	}
	other.release()
	gate.mu.Lock()
	grantedEarly := waiting.granted
	gate.mu.Unlock()
	if grantedEarly {
		t.Fatal("group passed while adopted holder still runs")
	}
	first.release()
	select {
	case release := <-granted:
		release()
	case <-time.After(time.Second):
		t.Fatal("group did not pass after completion")
	}
	if gate.used != 0 || gate.runs != 0 {
		t.Fatalf("admission leaked: %d/%d", gate.used, gate.runs)
	}
}

func TestAdoptedRunStillHonorsCancelAndSupersede(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "supersede"}[supersede], func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/preempted", "dddddddddddddddddddddddddddddddddddddddd")
			fixture.setJobName(t, run.ID, "oberth-preempted")
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitErr: errors.New("Workflow deleted"), waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
			scheduler := fixture.scheduler(t, jobs)
			reservation, err := scheduler.admission.adopt(run, periapsis.M)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scheduler.resumeAdoptedRun(ctx, run, scheduler.requireMutation, reservation) }()
			select {
			case <-jobs.waitEntered:
			case err := <-done:
				t.Fatalf("resume stopped: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Wait not entered")
			}
			if supersede {
				if _, err := fixture.store.EnqueueRun(context.Background(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: run.Ref, SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Actor: "agent@host", Trigger: "branch"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := fixture.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := scheduler.completePendingCancellations(context.Background()); err != nil {
				t.Fatal(err)
			}
			close(jobs.waitBlocks)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			current, err := fixture.store.Run(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !current.Preempted() || len(jobs.deleted) != 1 || jobs.deleted[0] != current.JobName {
				t.Fatalf("adopted cancellation = %#v deleted=%v", current, jobs.deleted)
			}
			if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
				t.Fatalf("pending cancellation = %#v %v", pending, err)
			}
			scheduler.admission.mu.Lock()
			used, runs := scheduler.admission.used, scheduler.admission.runs
			scheduler.admission.mu.Unlock()
			if used != 0 || runs != 0 {
				t.Fatalf("cancelled adoption retained admission: %d/%d", used, runs)
			}
		})
	}
}

func TestNewBranchWorkflowSurvivesShutdown(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/new-shutdown", "ffffffffffffffffffffffffffffffffffffffff")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
	scheduler := fixture.scheduler(t, jobs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.executeAndCleanup(ctx, run) }()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("execute stopped: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Wait not entered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunRunning || current.SupersededBy != "" || current.JobName == "" || jobs.created != 1 || len(jobs.deleted) != 0 {
		t.Fatalf("new Workflow shutdown = %#v created=%d deleted=%v", current, jobs.created, jobs.deleted)
	}
	if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("shutdown cancellation = %#v %v", pending, err)
	}
}

func TestAdoptionLeavesPromotionAndReleaseOnLegacyRecovery(t *testing.T) {
	for _, release := range []bool{false, true} {
		fixture := newRecoveryFixture(t)
		spec := model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/promotion", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host", Trigger: "promotion"}
		if release {
			spec.RefKind = model.RefTag
			spec.Ref = "v1"
			spec.Trigger = "tag"
			spec.Release = true
		}
		var enqueued model.EnqueueRunResult
		var err error
		if release {
			enqueued, err = fixture.store.EnqueueRun(context.Background(), spec)
		} else {
			spec.TestedSHA, spec.BaseSHA = spec.SHA, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			enqueued, _, err = fixture.store.EnqueuePromotionRun(context.Background(), spec, model.PromotionSpec{
				RepoID: fixture.repo.ID, SourceBranch: spec.Ref, SourceSHA: spec.SHA, TargetRef: "main", Actor: "agent@host",
				ResultSHA: spec.SHA, PreviousSHA: spec.BaseSHA,
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		run := enqueued.Run
		if _, err := fixture.store.ClaimNextRun(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.setJobName(t, run.ID, "oberth-legacy")
		jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
		scheduler := fixture.scheduler(t, jobs)
		if err := scheduler.reconcileStrandedRunsWithGate(context.Background(), scheduler.requireMutation, func(model.Run, periapsis.Size) error { return errors.New("out-of-scope adoption") }); err != nil {
			t.Fatal(err)
		}
		current, err := fixture.store.Run(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != model.RunInterrupted || current.SupersededBy != "" || len(jobs.adopted) != 0 || len(jobs.deleted) != 1 {
			t.Fatalf("legacy recovery changed: %#v adopted=%v deleted=%v", current, jobs.adopted, jobs.deleted)
		}
	}
}

func TestAdoptionPreemptedBeforeWaitRetainsAdmissionOnDeleteFailure(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/delete-failure", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fixture.setJobName(t, run.ID, "oberth-delete-failure")
	if err := fixture.store.SetRunConcurrencyGroup(context.Background(), run.ID, "build"); err != nil {
		t.Fatal(err)
	}
	run.ConcurrencyGroup = "build"
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{deleteErr: errors.New("API unavailable")}}
	scheduler := fixture.scheduler(t, jobs)
	reservation, err := scheduler.admission.adopt(run, periapsis.M)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.resumeAdoptedRun(context.Background(), run, scheduler.requireMutation, reservation); err == nil {
		t.Fatal("failed deletion accepted")
	}
	scheduler.admission.mu.Lock()
	used, runs, held := scheduler.admission.used, scheduler.admission.runs, scheduler.admission.groups[concurrencyGroupKey(run.RepoID, run.ConcurrencyGroup)]
	scheduler.admission.mu.Unlock()
	if used == 0 || runs != 1 || held != reservation || !reservation.retained {
		t.Fatalf("live Workflow lost admission: %d/%d held=%p", used, runs, held)
	}
}
