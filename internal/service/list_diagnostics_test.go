package service

import (
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// Cache-backed observations are available on the first response.
func pollDiagnosticList(t *testing.T, s *API, actor api.Actor, filter api.RunFilter, ready func([]model.Run) bool) []model.Run {
	t.Helper()
	value, err := s.Runs(t.Context(), actor, filter)
	if err != nil {
		t.Fatal(err)
	}
	rows := value.([]model.Run)
	if !ready(rows) {
		t.Fatal("inline cache observation missing", rows)
	}
	return rows
}
