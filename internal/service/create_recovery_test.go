package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Production ArgoJobs supports both interfaces. A settler-only fixture cannot
// catch recovery accidentally bypassing normal Workflow adoption.
type createRecoveryJobs struct {
	adoptingJobs
	present     map[string]bool
	settleCalls int
}

func (j *createRecoveryJobs) Adopt(_ context.Context, run model.Run) (periapsis.Size, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.adopted = append(j.adopted, run.ID)
	return periapsis.S, j.present[run.ID], nil
}
func (j *createRecoveryJobs) SettleCreate(context.Context, string, string, time.Time) (bool, error) {
	j.settleCalls++
	return false, nil
}

var _ JobAdopter = (*createRecoveryJobs)(nil)
var _ JobCreateSettler = (*createRecoveryJobs)(nil)

func recoveryWaitingRun(t *testing.T, f *recoveryFixture, ref, name, group string) model.Run {
	t.Helper()
	run := f.enqueueAndClaim(t, ref, strings.Repeat("a", 40))
	f.setJobName(t, run.ID, name)
	if group != "" {
		if err := f.store.SetRunConcurrencyGroup(t.Context(), run.ID, group); err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestWorkflowCreateRecoveryParkedGroupMateRequeuesInEitherOrder(t *testing.T) {
	for _, parkedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("parkedFirst=%t", parkedFirst), func(t *testing.T) {
			f := newRecoveryFixture(t)
			var holder, parked model.Run
			for _, park := range []bool{parkedFirst, !parkedFirst} {
				if park {
					parked = recoveryWaitingRun(t, f, "feature/parked", "wf-parked", "serial")
				} else {
					holder = recoveryWaitingRun(t, f, "feature/holder", "wf-holder", "serial")
					var err error
					holder, err = f.store.MarkRunSubmitted(t.Context(), holder.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			jobs := &createRecoveryJobs{present: map[string]bool{holder.ID: true}, adoptingJobs: adoptingJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}}
			scheduler := f.scheduler(t, jobs)
			if _, err := os.Stat(scheduler.createAdmissionPath(parked.ID)); !os.IsNotExist(err) {
				t.Fatal("fixture has a create contract", err)
			}
			adopted := 0
			if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(run model.Run, size periapsis.Size) error {
				adopted++
				if run.ID != holder.ID {
					t.Fatal("parked placeholder adopted")
				}
				_, err := scheduler.admission.adopt(run, size, argoworkflow.VMIDemand{})
				return err
			}); err != nil {
				t.Fatal("parked group-mate blocked startup", err)
			}
			current, err := f.store.Run(t.Context(), parked.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !current.Preempted() || current.SupersededBy == "" {
				t.Fatalf("never-posted run failed instead of requeueing: %+v", current)
			}
			replacement, err := f.store.Run(t.Context(), current.SupersededBy)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.Status != model.RunQueued || replacement.SHA != parked.SHA || replacement.Ref != parked.Ref {
				t.Fatalf("restart lost queued source: %+v", replacement)
			}
			if err := scheduler.completePendingCancellations(t.Context()); err != nil {
				t.Fatal(err)
			}
			state := scheduler.AdmissionState()
			if adopted != 1 || scheduler.pendingCreateCount() != 0 || state.Used != 1 || state.Runs != 1 || len(state.Groups) != 1 || state.Groups[0].Holder != holder.ID || jobs.settleCalls != 0 {
				t.Fatalf("placeholder took a grant/quarantine or displaced sibling: adopted=%d pending=%d state=%+v settles=%d", adopted, scheduler.pendingCreateCount(), state, jobs.settleCalls)
			}
			if len(jobs.deleted) != 1 || jobs.deleted[0] != parked.JobName {
				t.Fatalf("real sibling was deleted: %v", jobs.deleted)
			}
		})
	}
}

func TestWorkflowCreateRecoveryWaitingExistingWorkflowAdoptsBeforeCleanup(t *testing.T) {
	for _, contract := range []bool{false, true} {
		t.Run(fmt.Sprintf("contract=%t", contract), func(t *testing.T) {
			f := newRecoveryFixture(t)
			run := recoveryWaitingRun(t, f, "feature/created", "wf-created", "")
			jobs := &createRecoveryJobs{present: map[string]bool{run.ID: true}, adoptingJobs: adoptingJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}}
			scheduler := f.scheduler(t, jobs)
			if contract {
				if err := scheduler.recordCreateAdmission(run, run.JobName, periapsis.S, argoworkflow.VMIDemand{}); err != nil {
					t.Fatal(err)
				}
			}
			var resumed model.Run
			var grant *admissionReservation
			if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(found model.Run, size periapsis.Size) error {
				resumed = found
				var err error
				grant, err = scheduler.admission.adopt(found, size, argoworkflow.VMIDemand{})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if resumed.ID != run.ID || grant == nil || scheduler.pendingCreateCount() != 0 || jobs.settleCalls != 0 || len(jobs.deleted) != 0 {
				t.Fatalf("existing Workflow routed to deletion: resumed=%+v pending=%d deleted=%v", resumed, scheduler.pendingCreateCount(), jobs.deleted)
			}
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scheduler.resumeAdoptedRun(ctx, resumed, scheduler.requireMutation, grant) }()
			select {
			case <-jobs.waitEntered:
			case err := <-done:
				t.Fatalf("adopted observer stopped: %v", err)
			case <-time.After(testStartTimeout(5 * time.Second)):
				t.Fatal("adopted observer not started")
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Phase != "running" || current.Status != model.RunRunning || current.SupersededBy != "" {
				t.Fatalf("adoption failed to mark original run submitted: %+v", current)
			}
			if _, err := os.Stat(scheduler.createAdmissionPath(run.ID)); !os.IsNotExist(err) {
				t.Fatalf("adoption leaked acknowledged create contract: %v", err)
			}

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(testStartTimeout(5 * time.Second)):
				t.Fatal("adopted observer did not stop")
			}
			if jobs.settleCalls != 0 || len(jobs.deleted) != 0 {
				t.Fatal("adopted Workflow deleted at shutdown")
			}
		})
	}
}

func TestWorkflowCreateRecoveryAbsentWorkflowRequiresContractToSettle(t *testing.T) {
	for _, contract := range []string{"none", "recorded", "corrupt"} {
		t.Run(contract, func(t *testing.T) {
			f := newRecoveryFixture(t)
			run := recoveryWaitingRun(t, f, "feature/absent", "wf-absent", "")
			jobs := &createRecoveryJobs{present: map[string]bool{}, adoptingJobs: adoptingJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}}
			scheduler := f.scheduler(t, jobs)
			if contract != "none" {
				if err := scheduler.recordCreateAdmission(run, run.JobName, periapsis.S, argoworkflow.VMIDemand{}); err != nil {
					t.Fatal(err)
				}
				if contract == "corrupt" {
					if err := os.WriteFile(scheduler.createAdmissionPath(run.ID), []byte("incomplete"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(model.Run, periapsis.Size) error { t.Fatal("absent Workflow adopted"); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(jobs.adopted) != 1 || jobs.adopted[0] != run.ID || jobs.settleCalls != 0 {
				t.Fatal("recovery skipped adoption or settled inline")
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			state := scheduler.AdmissionState()
			if contract == "none" {
				if !current.Preempted() || current.SupersededBy == "" || state.Used != 0 || scheduler.pendingCreateCount() != 0 {
					t.Fatalf("no-contract run quarantined or failed: %+v state=%+v", current, state)
				}
				return
			}
			wantWeight := 1
			if contract == "corrupt" {
				wantWeight = state.Capacity
			}
			if current.Status != model.RunRunning || scheduler.pendingCreateCount() != 1 || state.Used != wantWeight {
				t.Fatalf("possible POST requeued or contract not reserved: %+v state=%+v", current, state)
			}
			scheduler.settleOneCreate(t.Context())
			current, err = f.store.Run(t.Context(), run.ID)
			if err != nil || !isPendingCreate(current) || jobs.settleCalls != 1 {
				t.Fatalf("absent possible POST lost background cleanup: %+v %v", current, err)
			}
		})
	}
}
