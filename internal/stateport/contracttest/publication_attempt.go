package contracttest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func checkPublicationAttemptEvidence(t *testing.T, f RunFixture) {
	p := moving(t, f)
	pub := model.Publication{ID: "empty-history", Repo: "origin/team/repo", Ref: "refs/heads/main", Actor: "operator", ObjectSHA: strings.Repeat("a", 40)}
	a := stateport.Action{Actor: pub.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	got, err := p.Publication(t.Context(), pub.ID)
	mustRun(t, err)
	if got.Attempted || got.CIAfter != 0 || got.Attempts != 0 {
		t.Fatal("new intent already has attempt evidence", got)
	}
	got, err = p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	requireAttemptEvidence(t, got, 0, 1)
	mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1}))
	mustRun(t, p.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID, ExpectedAttempts: 1}))
	got, err = p.Publication(t.Context(), pub.ID)
	mustRun(t, err)
	requireAttemptEvidence(t, got, 0, 0)
	// A later run must not rebind the first attempt's empty history position.
	acceptOneRun(t, f, RunCase{ID: "after-first-attempt"})
	got, err = p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	requireAttemptEvidence(t, got, 0, 1)
	got, err = p.BindPublication(t.Context(), a, pub.ID, "")
	mustRun(t, err)
	requireAttemptEvidence(t, got, 0, 1)
	// A different intent captures the now nonempty CI history independently.
	pub.ID = "nonempty-history"
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	got, err = p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	requireAttemptEvidence(t, got, 1, 1)
	acceptOneRun(t, f, RunCase{ID: "after-second-attempt"})
	mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1}))
	mustRun(t, p.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID, ExpectedAttempts: 1}))
	got, err = p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	requireAttemptEvidence(t, got, 1, 1)
}

func requireAttemptEvidence(t *testing.T, p model.Publication, after int64, attempts int) {
	t.Helper()
	if !p.Attempted || p.CIAfter != after || p.Attempts != attempts {
		t.Fatal("attempt state changed the CI bound or retry budget", p)
	}
	data, err := json.Marshal(p)
	mustRun(t, err)
	for _, private := range []string{"attempted", "Attempted", "ci_after", "CIAfter"} {
		if strings.Contains(string(data), private) {
			t.Fatal("private attempt evidence escaped in public JSON", string(data))
		}
	}
}
