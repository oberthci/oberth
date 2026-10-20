package app

import (
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestScopedAdmissionKeepsOnlyVisibleRepositoriesAndHidesTheBudget(t *testing.T) {
	state := &model.AdmissionState{
		Capacity: 20, Used: 20, Runs: 2,
		Reservations: []model.AdmissionReservation{
			{RunID: "mine", RepoID: 1, Repo: "cli", State: "waiting", Position: 2},
			{RunID: "theirs", RepoID: 2, Repo: "secret-repo", State: "retained"},
		},
		Groups:  []model.AdmissionGroup{{Name: "tf", RepoID: 2, Repo: "secret-repo", Holder: "theirs"}},
		Blocker: &model.AdmissionBlocker{RunID: "theirs", RepoID: 2},
	}
	upstreams := []model.Upstream{{ID: 10, Name: "codeberg"}, {ID: 11, Name: "github"}}
	repositories := []model.Repository{{ID: 1, UpstreamID: 10}, {ID: 2, UpstreamID: 11}}
	scoped := scopedAdmission(state, upstreams, repositories, api.Actor{Identity: "agent@host", Scope: []string{"codeberg"}})
	if scoped.Capacity != 0 || scoped.Used != 0 || scoped.Blocker != nil || len(scoped.Groups) != 0 ||
		len(scoped.Reservations) != 1 || scoped.Reservations[0].RunID != "mine" || scoped.Reservations[0].Position != 0 {
		t.Fatalf("scoped admission = %#v", scoped)
	}
}
