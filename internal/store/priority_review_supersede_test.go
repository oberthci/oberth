package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPriorityReviewExplicitPrioritySupersede(t *testing.T) {
	for _, actor := range []string{"owner", "other", "admin"} {
		t.Run(actor, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			admin := true
			spec := testRunSpec(repo.ID, "urgent", strings.Repeat("a", 40))
			spec.Actor = "owner"
			spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHighest, UnscopedAdmin: &admin}
			first, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			spec.Actor, spec.SHA, spec.Supersede = actor, strings.Repeat("b", 40), true
			if _, err = s.SetPriorityGrant(t.Context(), "admin", model.PriorityGrant{Identity: actor, MaxClass: model.PriorityHighest, Expires: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			admin = actor == "admin"
			next, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			old, err := s.Run(t.Context(), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if actor == "other" {
				if old.Status != model.RunQueued || old.SupersededBy != "" {
					t.Fatalf("other cancelled protected run: %+v", old)
				}
			} else if old.Status != model.RunInterrupted || old.SupersededBy != next.ID {
				t.Fatalf("explicit priority supersede ignored: %+v", old)
			}
		})
	}
}
