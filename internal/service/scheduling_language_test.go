package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type schedulingLanguageObserver struct{ observation *model.AdmissionObservation }

func (o schedulingLanguageObserver) AdmissionSnapshot(int64) *model.AdmissionObservation {
	return o.observation
}

func TestSchedulingMessagesUsePlainWords(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, observation := range []*model.AdmissionObservation{
			nil,
			{State: "preparing"},
			{State: "admitted"},
			{State: "waiting"},
			{State: "waiting", Requests: &argoworkflow.PodRequests{CPUMilli: 9900, MemoryBytes: 16 << 30}, Pressure: &model.PressureDeferral{Resource: "cpu"}},
			{State: "waiting", Pressure: &model.PressureDeferral{Resource: "io"}},
			{State: "waiting", Runs: 6, MaxRuns: 6},
			{State: "waiting", Group: "deploy", GroupHolder: "holder"},
			{State: "waiting", DiskFloor: &model.DiskFloorObservation{BelowFloor: true, FreeBytes: 512 << 20}},
			{State: "waiting"},
			{State: "waiting", BlockedBy: &model.AdmissionBlocker{Repo: "org/repo", Ref: "main"}},
			{State: "waiting", HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{RunCeiling: true}},
		} {
			control := &API{admission: schedulingLanguageObserver{observation}}
			if scoped {
				control.scope = &repoScope{}
			}
			for _, status := range []model.RunStatus{model.RunQueued, model.RunRunning} {
				run := model.Run{ID: "run", Status: status, QueueSequence: 1, Priority: model.PriorityHigh, PriorityReason: "declared effective capacity for admission", QueuedAt: time.Now()}
				got, reason := control.observeScheduling(t.Context(), run, nil, false)
				assertPlainScheduling(t, got.Message)
				assertPlainScheduling(t, reason)

			}
		}
	}
	for _, cause := range []string{store.ExecutorIdentityPendingMessage, store.AdmissionStalledPendingMessage} {
		got, _ := (&API{}).observeScheduling(t.Context(), model.Run{ID: "run", Status: model.RunRunning, Phase: "infrastructure-pending", Error: cause}, nil, false)
		assertPlainScheduling(t, got.Message)
		if !strings.Contains(got.Message, "timed out") {
			t.Fatal("startup timeout cause hidden", got.Message)
		}
	}
	// Detailed helper text may still be used outside a queued row.
	for _, observation := range []*model.AdmissionObservation{
		{},
		{DiskFloor: &model.DiskFloorObservation{BelowFloor: true}},
		{HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{}},
	} {
		assertPlainScheduling(t, waitingAdmissionMessage(observation))
	}
}

func assertPlainScheduling(t *testing.T, message string) {
	t.Helper()
	for _, word := range []string{"capacity", "admission", "declared", "effective"} {
		if strings.Contains(strings.ToLower(message), word) {
			t.Errorf("scheduling message contains %s: %q", word, message)
		}
	}
}

type structuredQueuePressure struct{ message queuedAdmissionMessage }

func (s structuredQueuePressure) AdmissionSnapshot(int64) *model.AdmissionObservation { return nil }
func (s structuredQueuePressure) QueuedAdmissionMessage() queuedAdmissionMessage      { return s.message }

func TestScopedQueueUsesStructuredPressureFlag(t *testing.T) {
	for _, tc := range []struct {
		text     string
		pressure bool
		want     string
	}{
		{"host busy (different wording)", true, "host busy (different wording)"},
		{"host CPU busy", false, "in line"},
	} {
		control := &API{scope: &repoScope{}, admission: structuredQueuePressure{queuedAdmissionMessage{Text: tc.text, Pressure: tc.pressure}}}
		_, reason := control.observeScheduling(t.Context(), model.Run{Status: model.RunQueued}, nil, false)
		if reason != tc.want {
			t.Fatalf("text determined scope visibility: %q", reason)
		}
	}
}
