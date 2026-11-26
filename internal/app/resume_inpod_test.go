package app_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestInPodResumeDormantGate(t *testing.T) {
	t.Parallel()
	_, s, n, old := appResumeFixture(t)
	old.Status = stepmodel.RunRunning
	_, err := s.ResumeRun(t.Context(), service.ResumeRequest{Run: old, Actor: request().Actor, ID: "resume", Commit: strings.Repeat("f", 40)})
	if err == nil || !strings.Contains(err.Error(), "assist unavailable") || n.applied {
		t.Fatal("in-Pod resume bypassed dormant gate", err, n.applied)
	}
}
