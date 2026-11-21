package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueLockStopsBlockingEditsAtExpiry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		offset time.Duration
		locked bool
	}{
		{"before", -time.Nanosecond, true},
		{"at", 0, false},
		{"after", time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			issue := manualIssue(t, s)
			requireOK(t, s.ChangeIssueLock(t.Context(), stateport.Action{Actor: "owner"}, stateport.IssueLockRequest{ID: issue.ID, Owner: "owner"}))
			before, err := s.Issue(t.Context(), issue.ID)
			requireOK(t, err)
			now := before.Lock.ExpiresAt.Add(tc.offset)
			s.options.Clock = func() time.Time { return now }
			got, err := s.UpdateIssue(t.Context(), stateport.Action{Actor: "other"}, model.IssuePatch{
				ID: issue.ID, ExpectedRevision: &issue.Revision, Title: new("updated"),
			})
			if tc.locked {
				want := "conflict: issue is locked by another actor - Wait for that lock to expire or ask its owner to release it."
				if err == nil || err.Error() != want {
					t.Fatalf("edit before expiry: %v; want %s", err, want)
				}
				got, err = s.Issue(t.Context(), issue.ID)
				requireOK(t, err)
				if !reflect.DeepEqual(got, before) {
					t.Fatal("locked edit changed the issue")
				}
				return
			}
			requireOK(t, err)
			stored, err := s.Issue(t.Context(), issue.ID)
			requireOK(t, err)
			if !reflect.DeepEqual(got, stored) || got.Title != "updated" || got.Revision != issue.Revision+1 || !got.UpdatedAt.Equal(now) {
				t.Fatalf("expired lock still blocked the edit: %#v", got)
			}
		})
	}
}
