package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestScopedAdmissionRedactsOtherRepositories(t *testing.T) {
	state := &model.AdmissionState{Reservations: []model.AdmissionReservation{{RunID: "visible", RepoID: 1}, {RunID: "hidden", RepoID: 2}}}
	got := scopedAdmission(state, []model.Upstream{{ID: 1, Name: "public"}, {ID: 2, Name: "private"}}, []model.Repository{{ID: 1, UpstreamID: 1}, {ID: 2, UpstreamID: 2}}, api.Actor{Scope: []string{"public"}})
	body, err := json.Marshal(got)
	if err != nil || strings.Contains(string(body), "hidden") || len(got.Reservations) != 1 {
		t.Fatal(string(body), err)
	}
	if len(state.Reservations) != 2 {
		t.Fatal("scoped read mutated shared state")
	}
}

func TestScopedAdmissionKeepsOnlyVisibleRepositoriesAndHidesTheLoad(t *testing.T) {
	state := &model.AdmissionState{
		Used: 20, Runs: 2,
		Pressure: &model.AdmissionPressure{Holding: true, Sample: &model.PressureSample{IO: model.PressureResource{FullAvg60: 80}}},
		Reservations: []model.AdmissionReservation{
			{RunID: "mine", RepoID: 1, Repo: "cli", State: "waiting", Position: 2, Pressure: &model.PressureDeferral{Resource: "io", Avg60: 80}},
			{RunID: "theirs", RepoID: 2, Repo: "secret-repo", State: "retained"},
		},
		Blocker: &model.AdmissionBlocker{RunID: "theirs", RepoID: 2},
	}
	upstreams := []model.Upstream{{ID: 10, Name: "codeberg"}, {ID: 11, Name: "github"}}
	repositories := []model.Repository{{ID: 1, UpstreamID: 10}, {ID: 2, UpstreamID: 11}}
	scoped := scopedAdmission(state, upstreams, repositories, api.Actor{Identity: "agent@host", Scope: []string{"codeberg"}})
	if scoped.Pressure != nil || (scoped.Reservations[0].Pressure == nil || !scoped.Reservations[0].Pressure.Redacted || scoped.Reservations[0].Pressure.Avg60 != 0) || scoped.Used != 0 || scoped.Blocker != nil ||
		len(scoped.Reservations) != 1 || scoped.Reservations[0].RunID != "mine" || scoped.Reservations[0].Position != 0 {
		t.Fatalf("scoped admission = %#v", scoped)
	}
}
