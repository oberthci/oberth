package service

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// Native Argo errors remain visible for both underlying lookup failures.
// hack/test-workflow-semaphore.py regenerates and checks these controller errors
// from the pinned module before checking their display.
func TestReviewerPinnedArgoSemaphoreHint(t *testing.T) {
	raw, err := os.ReadFile("testdata/semaphore-failures.json")
	if err != nil {
		t.Fatal(err)
	}
	var proof struct {
		ArgoVersion string `json:"argo_version"`
		Cases       []struct {
			Missing string `json:"missing"`
			Message string `json:"message"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &proof); err != nil {
		t.Fatal(err)
	}
	if proof.ArgoVersion != "v4.0.8" || len(proof.Cases) != 2 {
		t.Fatalf("incomplete pinned proof: %+v", proof)
	}
	seen := map[string]bool{}
	for _, tc := range proof.Cases {
		t.Run(tc.Missing, func(t *testing.T) {
			if tc.Missing != "configmap" && tc.Missing != "key" {
				t.Fatal("unknown failure", tc.Missing)
			}
			if seen[tc.Missing] {
				t.Fatal("duplicate failure", tc.Missing)
			}
			seen[tc.Missing] = true
			for _, phase := range []string{"job", "infrastructure", "test"} {
				run := model.Run{Status: model.RunFailed, Phase: phase, Error: tc.Message}
				hint := runRetryHint(run, nil)
				if hint == nil || hint.Cause == "synchronization_configuration" {
					t.Fatalf("native Argo error %q received a parsed hint: %+v", tc.Message, hint)
				}
				if !strings.Contains(failedRunDisplayReason(run), tc.Message) || run.Error != tc.Message {
					t.Fatalf("Argo error changed: %+v", run)
				}
			}
		})
	}
}
