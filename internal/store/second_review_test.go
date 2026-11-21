package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"modernc.org/sqlite"
)

// The second review's terminal-prefix reproducer must never acquire a receipt.
func TestSecondPartialBatchCannotComplete(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "partial-batch")
	nodes := make([]stateport.Node, 129)
	for i := range nodes {
		nodes[i] = stateport.Node{NodeID: fmt.Sprintf("node-%03d", i), Path: "build", Attempt: i + 1, Status: stepmodel.StepPassed, Exit: new(0), Credential: new(false)}
	}
	seq, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: r.ID, Nodes: len(nodes)})
	requireOK(t, err)
	requireOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: r.ID, Sequence: seq, Nodes: nodes[:128]}))
	execute(t, s.writer, `CREATE TRIGGER injected_failure BEFORE INSERT ON nodes WHEN NEW.node_id='node-128' BEGIN SELECT RAISE(ABORT,'injected second batch failure'); END`)
	batch := stateport.NodeBatch{RunID: r.ID, Sequence: seq, Offset: 128, Nodes: nodes[128:]}
	if err := s.PutNodes(t.Context(), stateport.Action{}, batch); err == nil {
		t.Fatal("fixture did not fail")
	}
	completion := stateport.EvidenceCompletion{RunID: r.ID, Sequence: seq}
	requireCode(t, s.CompleteEvidence(t.Context(), stateport.Action{}, completion), publicerr.Conflict)
	execute(t, s.writer, `DROP TRIGGER injected_failure`)
	requireOK(t, s.PutNodes(t.Context(), stateport.Action{}, batch))
	requireOK(t, s.CompleteEvidence(t.Context(), stateport.Action{}, completion))
	// An old worker cannot complete a newer observation, or replay its batch.
	_, err = s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: r.ID, Nodes: 129})
	requireOK(t, err)
	requireCode(t, s.CompleteEvidence(t.Context(), stateport.Action{}, completion), publicerr.Conflict)
	requireCode(t, s.PutNodes(t.Context(), stateport.Action{}, batch), publicerr.Conflict)
}

func TestSecondRetentionReportsInternalGap(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	at := s.options.Clock()
	e := stateport.EventWrite{Event: model.AgentEvent{Repo: "origin/team/repo", Kind: string(model.EventRunFinished), At: at}, Dedupe: "kept-before"}
	first, err := eventForTest(t.Context(), s, e)
	requireOK(t, err)
	e.Event.Kind = string(model.EventRunUpdated)
	e.Dedupe = "pruned-progress"
	_, err = eventForTest(t.Context(), s, e)
	requireOK(t, err)
	e.Event.Kind = string(model.EventRunFinished)
	e.Dedupe = "kept-after"
	last, err := eventForTest(t.Context(), s, e)
	requireOK(t, err)
	requireOK(t, s.RetainEvents(t.Context(), at.Add(-time.Hour), at.Add(time.Second)))
	page, err := s.Events(t.Context(), stateport.EventFilter{Repositories: []string{e.Event.Repo}, After: first, Limit: 200})
	requireOK(t, err)
	if !page.Gap || page.NextAfterID != last || len(page.Events) != 1 {
		t.Fatal("internal progress loss was hidden", page)
	}
	requireOK(t, s.RetainEvents(t.Context(), at.Add(time.Second), at.Add(time.Second)))
	page, err = s.Events(t.Context(), stateport.EventFilter{Repositories: []string{e.Event.Repo}, After: first, Limit: 200})
	requireOK(t, err)
	if !page.Gap || len(page.Events) != 0 || page.LatestID != last || page.NextAfterID != last || page.HasMore {
		t.Fatal("empty partition lost its pruning watermark", page)
	}
	resume, err := s.Events(t.Context(), stateport.EventFilter{Repositories: []string{e.Event.Repo}, After: page.NextAfterID, Limit: 200})
	requireOK(t, err)
	if resume.Gap || resume.HasMore {
		t.Fatal("empty gap cursor cannot resume", resume)
	}
	page, err = s.Events(t.Context(), stateport.EventFilter{Repositories: []string{"other/team/repo"}, After: first, Limit: 200})
	requireOK(t, err)
	if page.Gap {
		t.Fatal("another repository's pruning leaked into scope")
	}
}

func TestSecondArchiveAfterCompletedObligation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "finished")
	a := stateport.Action{Actor: "operator"}
	// Completed obligations are read facts; native settlement has no store port.
	execute(t, s.writer, `INSERT INTO obligations VALUES('finished','stop',2,'','operator')`)
	r, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunCancelled}))
	idle, err := s.Idle(t.Context())
	requireOK(t, err)
	if !idle {
		t.Fatal("fixture is not idle")
	}
	requireOK(t, s.ArchiveRepository(t.Context(), a, r.RepositoryID))
}

func TestSnapshotRechecksSpaceAfterTheLastCopyStep(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	config := defaultSnapshot(1 << 30)
	free := int64(1 << 40)
	config.free = func(string) (int64, error) { return free, nil }
	config.step = func(b *sqlite.Backup) (bool, error) { more, err := b.Step(-1); free = 0; return more, err }
	backup := &Backup{path: s.path, snapshot: &config}
	_, err := backup.Snapshot(t.Context(), backupOptions(s), io.Discard)
	requireCode(t, err, publicerr.Unavailable)
}

func TestOpenDuringSnapshotAndUnownedStage(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	parent := filepath.Dir(s.path)
	lock, err := lockParent(parent)
	requireOK(t, err)
	defer func() { requireOK(t, lock.Close()) }()
	path := filepath.Join(parent, stagePrefix+"unowned")
	requireOK(t, os.Mkdir(path, 0755))
	requireOK(t, os.Chmod(path, 0755))
	requireOK(t, os.WriteFile(filepath.Join(path, "owner"), []byte("oberth.state-stage/v1\n"), 0600))
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	requireOK(t, second.Close())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unowned directory was removed", err)
	}
}
