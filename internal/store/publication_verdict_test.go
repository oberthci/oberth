package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunPublicationRequiresPassedVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		finish stepmodel.RunStatus
	}{
		{"unfinished", ""}, {"failed", stepmodel.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			r, _ := accepted(t, s, "run")
			a := stateport.Action{Actor: "operator"}
			if tc.finish != "" {
				requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: tc.finish}))
			}
			_, head, err := s.Audit(t.Context(), 0, 100)
			requireOK(t, err)
			changed := observeChanges(t, s)
			intent := publicationIntent(r)
			err = s.PreparePublication(t.Context(), a, intent)
			want := "forbidden: authority is missing or expired - Request an active grant or an explicit scope change."
			if err == nil || err.Error() != want {
				t.Fatalf("publication without passed verdict: %v; want %s", err, want)
			}
			_, err = s.Publication(t.Context(), intent.Publication.ID)
			requireCode(t, err, publicerr.NotFound)
			_, after, err := s.Audit(t.Context(), 0, 100)
			requireOK(t, err)
			if after != head {
				t.Fatal("refused publication changed the audit head")
			}
			select {
			case <-changed:
				t.Fatal("refused publication notified a change")
			default:
			}
		})
	}
}
