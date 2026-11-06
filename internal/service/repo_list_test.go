package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestRepoListReturnsOnlySlimScopedRecords(t *testing.T) {
	f := newScopeFixture(t)
	value, err := f.call(scopedActor, "repo_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := value.(api.RepoListResponse).Repositories.([]repoSummary)
	if len(rows) != 1 || rows[0].ID != f.visible.ID || rows[0].Upstream != "github" || rows[0].LatestRun == nil {
		t.Fatalf("%+v", rows)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Repositories []map[string]json.RawMessage `json:"repositories"`
	}
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Repositories[0]) != 6 {
		t.Fatalf("not slim: %s", raw)
	}
	var latest map[string]json.RawMessage
	if err = json.Unmarshal(decoded.Repositories[0]["latest_run"], &latest); err != nil || len(latest) != 4 {
		t.Fatalf("latest: %s %v", raw, err)
	}
	if strings.Contains(string(raw), hiddenRepoName) || strings.Contains(string(raw), "LatestRuns") {
		t.Fatal(string(raw))
	}
	full, err := f.service.Repositories(t.Context(), scopedActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.([]RepoView)[0].LatestRuns) < 2 {
		t.Fatal("dashboard history lost")
	}
}
