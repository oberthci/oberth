package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestStartRunUsesTheAcceptedIDWithoutDispatchOrder(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	var runs []model.Run
	for _, trigger := range []string{"branch", "schedule", "promotion"} {
		result, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/" + trigger, SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: trigger})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, result.Run)
	}
	for i := len(runs) - 1; i >= 0; i-- {
		started, err := s.StartRun(t.Context(), runs[i].ID)
		if err != nil || started.ID != runs[i].ID || started.Status != model.RunRunning {
			t.Fatalf("start=%+v %v", started, err)
		}
	}
}
