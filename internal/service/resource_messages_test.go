package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestResourceMessageKeepsDetailedDeadlineFailure(t *testing.T) {
	raw := model.ExecutionDeadlineMessage + " Step step-12345678/step-abc12345: deadline limit 60s; sampled memory peak unknown. Inspect oberth run resources --run " + strings.Repeat("a", 32) + " --include-failed; change deadlines in git."
	got := presentationRun(model.Run{ID: strings.Repeat("a", 32), Status: model.RunFailed, Error: raw})
	if got.DisplayStatus != "failed" || got.DisplayReason != "infrastructure error: deadline exceeded" || got.Error != raw {
		t.Fatalf("failure guidance lost: %+v", got)
	}
}
