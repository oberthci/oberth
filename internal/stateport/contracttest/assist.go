package contracttest

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/stateport"
	"reflect"
	"strings"
	"testing"
)

func assistIntent(r stateport.Run) stateport.HoldAction {
	return stateport.HoldAction{RunID: r.ID, Step: "build", NodeID: "node", PodUID: "pod-uid", Kind: "exec", Revision: r.Revision, Argv: []string{"diagnose", "build"}, TimeoutS: 60}
}
func HoldStore(t *testing.T, p HoldPort, r stateport.Run) {
	t.Helper()
	a := stateport.Action{Actor: r.Origin.Actor, Reason: "inspection audit"}
	q := assistIntent(r)
	checkAssistInputs(t, p, r)
	q.Revision++
	if p.Assist(t.Context(), a, q) == nil {
		t.Fatal("stale inspection audit accepted")
	}
	q.Revision = r.Revision
	mustRun(t, p.Assist(t.Context(), a, q))
	if p.Assist(t.Context(), a, q) == nil {
		t.Fatal("inspection audit revision reused")
	}
}
func checkMovingHolds(t *testing.T, f RunFixture) {
	p := moving(t, f)
	b := batchFor(t, f, RunCase{ID: "inspect-run"})
	b.Runs[0].Request.Assist = true
	rows, err := p.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	r := rows.Runs[0]
	q := assistIntent(r)
	q.Kind, q.Argv, q.TimeoutS = "abandon", nil, 0
	mustRun(t, p.Assist(t.Context(), stateport.Action{Actor: r.Origin.Actor}, q))
	r, err = p.Run(t.Context(), r.ID)
	mustRun(t, err)
	if r.Origin.Assisted {
		t.Fatal("release guessed an inspection outcome")
	}
	HoldStore(t, p, r)
	requireMovingAudit(t, f, "run.assist")
	for _, a := range f.Audit(t) {
		if a.Action != "run.assist" {
			continue
		}
		var got stateport.HoldAudit
		if json.Unmarshal([]byte(a.Details), &got) == nil && reflect.DeepEqual(got, stateport.HoldAudit{Kind: "exec", Step: "build", Reason: "inspection audit", NodeID: "node", PodUID: "pod-uid", Argc: 2, ArgvSHA256: "6d0dfca5fcd71a261a1aebcf13aa4f8c31b04745a9c2b3c75d4f4d0aba3bf7f3"}) {
			if strings.Contains(a.Details, "diagnose") {
				t.Fatal("raw argv in audit", a)
			}
			return
		}
	}
	t.Fatal("inspection audit omitted attempt, count or digest")
}
func checkAssistRollback(t *testing.T, f RunFixture) {
	p := moving(t, f)
	b := batchFor(t, f, RunCase{ID: "inspect-rollback"})
	b.Runs[0].Request.Assist = true
	rows, err := p.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	r := rows.Runs[0]
	movingFails(t, f, func(a stateport.Action) error { return p.Assist(t.Context(), a, assistIntent(r)) })
	got, err := p.Run(t.Context(), r.ID)
	mustRun(t, err)
	if got.Origin.Assisted || got.Revision != r.Revision {
		t.Fatal("refused audit escaped transaction", got)
	}
}

func checkAssistInputs(t *testing.T, p HoldPort, r stateport.Run) {
	t.Helper()
	a := stateport.Action{Actor: r.Origin.Actor}
	for _, change := range []func(*stateport.HoldAction){
		func(q *stateport.HoldAction) { q.Kind = "continue" },
		func(q *stateport.HoldAction) { q.Kind = "patch" },
		func(q *stateport.HoldAction) { q.NodeID = "" },
		func(q *stateport.HoldAction) { q.PodUID = "" },
		func(q *stateport.HoldAction) { q.Step = "unadmitted" },
		func(q *stateport.HoldAction) { q.Argv = nil },
		func(q *stateport.HoldAction) { q.Argv = []string{""} },
		func(q *stateport.HoldAction) { q.Argv = make([]string, 257); q.Argv[0] = "true" },
		func(q *stateport.HoldAction) { q.TimeoutS = 0 },
		func(q *stateport.HoldAction) { q.TimeoutS = 901 },
	} {
		q := assistIntent(r)
		change(&q)
		if err := p.Assist(t.Context(), a, q); err == nil {
			t.Fatal("invalid inspection audit accepted", q)
		}
	}
	if p.Assist(t.Context(), stateport.Action{}, assistIntent(r)) == nil {
		t.Fatal("inspection audit accepted an empty actor")
	}
}
