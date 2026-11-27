package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeProgressReusesPreparedQueries(t *testing.T) {
	s := testStore(t)
	r, _ := accepted(t, s, "prepared-progress")
	probe := probeProgressWriter(t, s)
	probe.takePrepares()
	for _, status := range []stepmodel.StepStatus{stepmodel.StepRunning, stepmodel.StepQueued} {
		requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: status}}}))
	}
	cached := map[string]bool{
		runReadPrefix + "admission" + runReadSuffix:                                                   true,
		`SELECT value FROM native_steps WHERE run_id=?`:                                               true,
		`INSERT INTO native_steps VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET value=excluded.value`: true,
		`UPDATE runs SET status=?,summary=? WHERE id=?`:                                               true,
		`SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='events'),0)+1`:                  true,
	}
	for _, query := range probe.takePrepares() {
		if cached[query] {
			t.Fatal("progress reparsed a cached query", query)
		}
	}
}

func TestNativeProgressPreparedReadsStayFreshAndHonorDDL(t *testing.T) {
	s := testStore(t)
	r, _ := accepted(t, s, "fresh-progress")
	p := stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepRunning}}}
	requireOK(t, s.SaveProgress(t.Context(), r.ID, p))
	before, err := s.Steps(t.Context(), r.ID)
	requireOK(t, err)
	var admission []byte
	requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT admission FROM runs WHERE id=?`, r.ID).Scan(&admission))
	_, err = s.writer.ExecContext(t.Context(), `UPDATE runs SET admission=NULL WHERE id=?`, r.ID)
	requireOK(t, err)
	p.Steps[0].Status = stepmodel.StepQueued
	if err := s.SaveProgress(t.Context(), r.ID, p); err == nil {
		t.Fatal("prepared read reused stale admission")
	}
	_, err = s.writer.ExecContext(t.Context(), `UPDATE runs SET admission=? WHERE id=?`, admission, r.ID)
	requireOK(t, err)
	_, err = s.writer.ExecContext(t.Context(), `CREATE TRIGGER progress_denied BEFORE UPDATE ON native_steps BEGIN SELECT RAISE(ABORT,'progress denied'); END`)
	requireOK(t, err)
	if err := s.SaveProgress(t.Context(), r.ID, p); err == nil {
		t.Fatal("prepared projection bypassed new trigger")
	}
	after, err := s.Steps(t.Context(), r.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected projection changed durable history")
	}
}
