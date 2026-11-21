package argojob

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type recoveryRunsPort = Runs
type recoveryRows struct {
	recoveryRunsPort
	rows     []stateport.Run
	stage    string
	badReads int
	running  stateport.Run
	statuses []stepmodel.RunStatus
}

func (r *recoveryRows) Runs(_ context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	r.statuses = append(r.statuses, q.Status)
	if q.Status == stepmodel.RunRunning && r.running.ID != "" {
		return []stateport.Run{r.running}, nil
	}
	if q.Status == stepmodel.RunQueued {
		return r.rows, nil
	}
	return nil, nil
}
func (r *recoveryRows) failure(id, stage string) error {
	if id == "bad" && r.stage == stage {
		r.badReads++
		return errors.New(stage + " unreadable")
	}
	return nil
}
func (r *recoveryRows) Run(_ context.Context, id string) (stateport.Run, error) {
	if id == r.running.ID {
		return r.running, nil
	}
	for _, row := range r.rows {
		if row.ID == id {
			return row, r.failure(id, "Run")
		}
	}
	panic("unexpected recovery id")
}
func (r *recoveryRows) Steps(_ context.Context, id string) ([]stepmodel.RunStep, error) {
	return nil, r.failure(id, "Steps")
}

type recoveryFacts struct {
	*reviewR2Progress
	rows *recoveryRows
}

func (p recoveryFacts) Nodes(_ context.Context, id string) ([]stateport.Node, error) {
	return nil, p.rows.failure(id, "Nodes")
}
func newRecoveryRows(stage string) *recoveryRows {
	rows := &recoveryRows{stage: stage}
	for _, id := range []string{"bad", "healthy"} {
		rows.rows = append(rows.rows, stateport.Run{RunSummary: stepmodel.RunSummary{ID: id, Status: stepmodel.RunQueued}, Origin: model.RunOrigin{WorkflowName: id, WorkflowUID: id + "-uid"}, Admission: admissioncontract.PreparedView{RunID: id}})
	}
	rows.running = rows.rows[1].Clone()
	rows.running.ID, rows.running.Status = "running-healthy", stepmodel.RunRunning
	rows.running.Origin.WorkflowName, rows.running.Origin.WorkflowUID = "running-healthy", "running-uid"
	rows.running.Admission.RunID = "running-healthy"
	return rows
}

func TestEachRecoveryReadFailureIsIsolatedAndLoggedOnce(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"Run", "Steps", "Nodes"} {
		t.Run(stage, func(t *testing.T) {
			var log bytes.Buffer
			rows := newRecoveryRows(stage)
			e := New(Config{Runs: rows, Progress: recoveryFacts{&reviewR2Progress{}, rows}, Logger: slog.New(slog.NewJSONHandler(&log, nil))})
			defer e.queue.ShutDown()
			for range 2 {
				if err := e.recover(t.Context()); err == nil {
					t.Fatal("incomplete recovery was not reported", err)
				}
			}
			if e.tracked["running-healthy"] == nil || !slices.Equal(rows.statuses, []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning, stepmodel.RunQueued, stepmodel.RunRunning}) || e.tracked["healthy"] == nil || e.tracked["bad"] != nil || rows.badReads != 2 || strings.Count(log.String(), `"run_id":"bad"`) != 1 {
				t.Fatal("bad row blocked or flooded recovery", rows.badReads, log.String())
			}
			rows.stage = ""
			if err := e.recover(t.Context()); err != nil || e.tracked["bad"] == nil {
				t.Fatal("next recovery did not retry row", err)
			}
		})
	}
}
