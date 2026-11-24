package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeProgressUsesOneProjectionRead(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "progress-queries")
	s.stopUses()
	<-s.usesDone
	probe := new(queryProbe)
	original := s.writer
	db := sql.OpenDB(probeConnector{driver: original.Driver(), dsn: "file:" + s.path + "?mode=rw&_txlock=immediate&_pragma=foreign_keys(1)", probe: probe})
	db.SetMaxOpenConns(1)
	s.writer = db
	defer func() { s.writer = original; requireOK(t, db.Close()) }()
	entry := r.Admission.Plan.Entries[0]
	started := r.CreatedAt.Add(time.Second)
	progress := stateport.Progress{Steps: []stepmodel.RunStep{{Path: entry.Path, Name: entry.Name, Status: stepmodel.StepRunning, StartedAt: &started}}}
	requireOK(t, s.SaveProgress(t.Context(), r.ID, progress))
	if queries := probe.take(); len(queries) > 4 {
		t.Fatal("progress reread a known projection", queries)
	}
	saved, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if saved.Status != stepmodel.RunRunning || saved.Running != 1 || saved.StartedAt == nil || !saved.StartedAt.Equal(started) || saved.Revision != r.Revision {
		t.Fatal("single-read progress lost the committed projection", saved)
	}
	changed := s.Changes()
	requireOK(t, s.SaveProgress(t.Context(), r.ID, progress))
	if queries := probe.take(); len(queries) > 2 || changed != s.Changes() {
		t.Fatal("repeated progress reread or committed unchanged facts", queries)
	}
}
