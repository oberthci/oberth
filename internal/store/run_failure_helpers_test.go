package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func infrastructureFailureForTest() model.RunResult {
	return model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "workflow unavailable"}
}
func queuedRunForTest(t *testing.T, s *Store, repoID int64, ref string) model.Run {
	t.Helper()
	r, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repoID, RefKind: model.RefBranch, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	return r.Run
}
