package service

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// The reviewer's real-Argo reproducer, covering both underlying lookup failures.
// hack/test-workflow-semaphore.py regenerates and checks these controller errors
// from the pinned module before running this hint assertion.
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
				if hint == nil || hint.Cause != "synchronization_configuration" || hint.Class != model.RetryCode {
					t.Fatalf("actual pinned Argo error %q still gets hint %+v", tc.Message, hint)
				}
				for _, word := range []string{"operator", "ConfigMap", "key", "namespace", "Workflow YAML"} {
					if !strings.Contains(hint.Action, word) {
						t.Fatalf("remedy missing %s: %+v", word, hint)
					}
				}
				if strings.Contains(hint.Action, "run_retry") || run.Status != model.RunFailed || run.Phase != phase || run.Error != tc.Message {
					t.Fatalf("hint changed lifecycle or retained generic retry: %+v, %+v", run, hint)
				}
			}
		})
	}
}
