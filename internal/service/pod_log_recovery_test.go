package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type journalRecoveryJobs struct {
	recoveryJobs
	t              *testing.T
	wantLost       bool
	noWrite        bool
	failProjection bool
	block          bool
	resumed        bool
	entered        chan struct{}
	observed       bool
}

func (*journalRecoveryJobs) Adopt(context.Context, model.Run) (periapsis.Size, bool, error) {
	return periapsis.M, true, nil
}
func (*journalRecoveryJobs) JobNamespace() string { return "oberth-argo" }
func (j *journalRecoveryJobs) WaitResumed(ctx context.Context, name string, out io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	j.resumed = true
	return j.Wait(ctx, name, out)
}
func (j *journalRecoveryJobs) Wait(ctx context.Context, _ string, out io.Writer) (JobResult, error) {
	j.observed = true
	state, ok := out.(interface{ PodLogContinuityLost() bool })
	if !ok || state.PodLogContinuityLost() != j.wantLost {
		j.t.Errorf("durability lost=%v, want %v", ok && state.PodLogContinuityLost(), j.wantLost)
	}
	if j.failProjection {
		writer := out.(*stepProgressLogWriter)
		writer.destination = &partialPodProjection{file: writer.logFile}
	}
	if !j.noWrite {
		sink, ok := out.(runlog.PodLogSink)
		if !ok {
			j.t.Error("missing pod sink")
		} else {
			c := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: time.Now().UTC(), Count: 1, Bytes: 27}
			if err := sink.CommitPodLog(c, []byte("[test/unit] new real output\n")); err != nil {
				if j.failProjection && errors.Is(err, runlog.ErrPodLogProjection) {
					return j.terminalResult, err
				}
				j.t.Errorf("retained output failed: %v", err)
			}
		}
	}
	if j.entered != nil {
		close(j.entered)
	}
	if j.block {
		<-ctx.Done()
		return JobResult{}, ctx.Err()
	}
	return j.terminalResult, nil
}

type closedResumeLogStore struct{ *runlog.Store }

func (s closedResumeLogStore) Resume(id string) (*os.File, []runprogress.Event, int64, error) {
	f, e, n, err := s.Store.Resume(id)
	if err == nil {
		_ = f.Close()
	}
	return f, e, n, err
}

func TestSchedulerPodJournalFailureDegradesWithoutStrandingRun(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		for _, kind := range []string{"healthy", "projection-write", "corrupt", "unopenable", "oversize", "projection-mismatch", "stat-failure"} {
			mode := "new"
			if adopted {
				mode = "adopted"
			}
			t.Run(mode+"/"+kind, func(t *testing.T) {
				fixture := newRecoveryFixture(t)
				run := fixture.enqueueAndClaim(t, "feature/journal", strings.Repeat("a", 40))
				fixture.setJobName(t, run.ID, "oberth-journal")
				if _, err := fixture.store.MarkRunSubmitted(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
				run, err := fixture.store.Run(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				result := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}
				jobs := &journalRecoveryJobs{t: t, wantLost: kind != "healthy" && kind != "projection-write", failProjection: kind == "projection-write", noWrite: kind == "stat-failure", recoveryJobs: recoveryJobs{terminalResult: result}}
				if kind == "stat-failure" {
					jobs.terminalErr = errors.New("API later unavailable")
					jobs.deleteErr = errors.New("cleanup later unavailable")
				}
				scheduler := fixture.scheduler(t, jobs)
				file, err := scheduler.logs.Create(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("[test/unit] retained before recovery\n"); err != nil {
					t.Fatal(err)
				}
				journalPath := file.Name() + ".pods.jsonl"
				switch kind {
				case "corrupt":
					if err := os.WriteFile(journalPath, []byte("{\"garbage\":1}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				case "unopenable":
					if err := os.Mkdir(journalPath, 0700); err != nil {
						t.Fatal(err)
					}
				case "oversize":
					f, err := os.Create(journalPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.Truncate(scheduler.maxRunLogBytes*2 + (8 << 20) + 1); err != nil {
						t.Fatal(err)
					}
					_ = f.Close()
				case "projection-mismatch":
					// Legacy v1 journals did not record plain prefix gaps. Model a hard
					// reset losing that unrecorded range: it must degrade, never guess it.
					keep, _ := file.Seek(0, io.SeekCurrent)
					gap := []byte("oberth: legacy unjournaled prefix\n")
					if _, err := file.Write(gap); err != nil {
						t.Fatal(err)
					}
					offset, _ := file.Seek(0, io.SeekCurrent)
					body := []byte("[test/unit] original tail\n")
					if _, err := file.Write(body); err != nil {
						t.Fatal(err)
					}
					c := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: time.Now().UTC(), Count: 1, Bytes: int64(len(body))}
					frame := struct {
						Version int                 `json:"version"`
						Offset  int64               `json:"offset"`
						Cursor  runlog.PodLogCursor `json:"cursor"`
						Data    []byte              `json:"data"`
						SHA256  string              `json:"sha256,omitempty"`
					}{Version: 1, Offset: offset, Cursor: c, Data: body}
					unsigned, _ := json.Marshal(frame)
					digest := sha256.Sum256(unsigned)
					frame.SHA256 = hex.EncodeToString(digest[:])
					raw, _ := json.Marshal(frame)
					if err := os.WriteFile(journalPath, append(raw, '\n'), 0600); err != nil {
						t.Fatal(err)
					}
					if err := file.Truncate(keep); err != nil {
						t.Fatal(err)
					}

				}
				reservation, err := scheduler.admission.adopt(run, periapsis.M, argoworkflow.VMIDemand{})
				if err != nil {
					t.Fatal(err)
				}
				defer reservation.cancel()
				if adopted {
					_ = file.Close()
					if kind == "stat-failure" {
						scheduler.logs = closedResumeLogStore{scheduler.logs.(*runlog.Store)}
					}
					err = scheduler.resumeAdoptedRun(context.Background(), run, scheduler.requireMutation, reservation)
				} else {
					if kind == "stat-failure" {
						_ = file.Close()
					}
					err = scheduler.observeRun(context.Background(), run, fixture.repo, scheduler.requireMutation, reservation, runObservation{file: file})
				}
				if err != nil {
					t.Fatalf("optional journal escaped to dispatcher: %v", err)
				}
				finished, err := fixture.store.Run(context.Background(), run.ID)
				if err != nil || finished.Status != model.RunPassed || !jobs.observed || jobs.resumed != adopted || len(jobs.deleted) != 0 {
					t.Fatalf("run stranded/rewritten: run=%+v observed=%v resumed=%v deleted=%v err=%v", finished, jobs.observed, jobs.resumed, jobs.deleted, err)
				}
				if kind != "healthy" && kind != "projection-write" {
					action, auditErr := fixture.store.LatestAuditActionByType(context.Background(), "run.log-continuity.lost", "run")
					if auditErr != nil || action.ResourceID != run.ID {
						t.Fatal("run continuity loss not durably marked", action, auditErr)
					}
				}
				body, err := os.ReadFile(file.Name())
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), "retained before recovery") {
					t.Fatal("retained prefix lost", string(body))
				}
				if kind != "stat-failure" && strings.Count(string(body), "] new real output\n") != 1 {
					t.Fatal("output lost or duplicated", string(body))
				}
				if kind != "healthy" && kind != "projection-write" && kind != "stat-failure" && !strings.Contains(string(body), runlog.PodLogContinuityLostMarker) {
					t.Fatal("continuity loss not marked", string(body))
				}
				_, statErr := os.Stat(journalPath)
				if kind == "unopenable" {
					if statErr != nil {
						t.Fatal("nonregular path unexpectedly removed", statErr)
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("terminal journal not pruned", statErr)
				}
			})
		}
	}
}

func TestSchedulerActiveOwnerRetainsPodJournalForRestart(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/owner", strings.Repeat("b", 40))
	fixture.setJobName(t, run.ID, "oberth-owner")
	if _, err := fixture.store.MarkRunSubmitted(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	jobs := &journalRecoveryJobs{t: t, block: true, entered: make(chan struct{})}
	scheduler := fixture.scheduler(t, jobs)
	reservation, err := scheduler.admission.adopt(run, periapsis.M, argoworkflow.VMIDemand{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.resumeAdoptedRun(ctx, run, scheduler.requireMutation, reservation) }()
	select {
	case <-jobs.entered:
	case err := <-done:
		t.Fatalf("observation stopped: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("observation not entered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil || current.Status != model.RunRunning {
		t.Fatal("owner did not remain adoptable", current, err)
	}
	file, _, _, err := scheduler.logs.Resume(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := os.Stat(file.Name() + ".pods.jsonl"); err != nil {
		t.Fatal("active journal pruned", err)
	}
	journal, err := runlog.OpenPodLogJournal(file, scheduler.maxRunLogBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if len(journal.Cursors()) != 1 {
		t.Fatal("active durable cursor lost")
	}
}

func TestSchedulerPendingCancellationKeepsPodJournal(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/cancel-owner", strings.Repeat("c", 40))
	fixture.setJobName(t, run.ID, "oberth-cancel-owner")
	if _, err := fixture.store.MarkRunSubmitted(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}), waitErr: errors.New("watch ended"), deleteErr: errors.New("cleanup API unavailable")}}
	scheduler := fixture.scheduler(t, jobs)
	f, err := scheduler.logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name() + ".pods.jsonl"
	j, err := runlog.OpenPodLogJournal(f, scheduler.maxRunLogBytes)
	if err != nil {
		t.Fatal(err)
	}
	c := runlog.PodLogCursor{Key: "workflow/node/pod", WorkflowUID: "workflow", Node: "node", Pod: "pod", Container: "main", At: time.Now().UTC(), Count: 1, Bytes: 21}
	if err := j.Commit(c, []byte("[test/unit] retained\n"), f.Write); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	_ = f.Close()
	reservation, err := scheduler.admission.adopt(run, periapsis.M, argoworkflow.VMIDemand{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- scheduler.resumeAdoptedRun(context.Background(), run, scheduler.requireMutation, reservation)
	}()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("owner stopped: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("Wait not entered")
	}
	if _, err := fixture.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}
	close(jobs.waitBlocks)
	if err := <-done; err == nil {
		t.Fatal("uncertain physical cleanup hidden")
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil || !current.Status.Terminal() {
		t.Fatal(current, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("terminal DB row with live cleanup lost recovery journal", err)
	}
}
