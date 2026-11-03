package store

import (
	"github.com/oberthci/oberth/internal/model"
	"strings"
	"testing"
	"time"
)

func TestPriorityReviewRecoveryKeepsOwnPlaceAndSiblings(t *testing.T) {
	for _, mode := range []string{"KVM", "infrastructure"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			intent := kvmIntent(t, s)
			source, err := s.Run(t.Context(), intent.RunID)
			if err != nil {
				t.Fatal(err)
			}
			source, err = s.BumpRunPriority(t.Context(), "owner", source.ID, model.PriorityHighest, "recover me")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "KVM" {
				reserveRestartPreflight(t, s, intent, true)
			}
			sibling, err := s.EnqueueRun(t.Context(), testRunSpec(source.RepoID, source.Ref, strings.Repeat("c", 40)))
			if err != nil {
				t.Fatal(err)
			}
			var next model.Run
			if mode == "KVM" {
				next, err = s.RecoverKVMPreflight(t.Context(), source.ID)
			} else {
				var failed model.Run
				failed, err = s.FinishRun(t.Context(), source.ID, autoRetryResult())
				if err == nil && failed.AutoRetriedBy != "" {
					next, err = s.Run(t.Context(), failed.AutoRetriedBy)
				}
			}
			if err != nil || next.ID == "" || next.AdmissionSequence != source.AdmissionSequence || next.Priority != model.PriorityHighest || next.PriorityReason != source.PriorityReason {
				t.Fatalf("lost protected recovery/place: %+v %v", next, err)
			}
			got, err := s.Run(t.Context(), sibling.ID)
			if err != nil || got.Status != model.RunQueued || got.SupersededBy != "" {
				t.Fatalf("recovery replaced sibling: %+v %v", got, err)
			}
		})
	}
}
