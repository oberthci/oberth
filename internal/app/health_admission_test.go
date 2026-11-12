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
