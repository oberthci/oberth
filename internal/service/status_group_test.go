package service

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestStatusBuildersKeepConcurrencyGroup(t *testing.T) {
	s := &API{}
	repo := model.Repository{Name: "oberth"}
	run := model.Run{ID: "run", Ref: "main", Status: model.RunPassed, ConcurrencyGroup: "deploy"}
	exact, err := s.statusForRun(t.Context(), repo, run)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := s.statusFromRun(t.Context(), repo, run, "main", "reader")
	if err != nil {
		t.Fatal(err)
	}
	if branch.ConcurrencyGroup != "deploy" || branch.ConcurrencyGroup != exact.ConcurrencyGroup {
		t.Fatalf("branch group %q, exact group %q", branch.ConcurrencyGroup, exact.ConcurrencyGroup)
	}
}
