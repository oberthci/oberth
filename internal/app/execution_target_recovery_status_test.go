package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type targetRecoveryHealthStore struct{ fakeHealthStore }

func (targetRecoveryHealthStore) ExecutionTargetRecoveryStatus(context.Context) (*model.ExecutionTargetRecoveryStatus, error) {
	return &model.ExecutionTargetRecoveryStatus{QuarantinedBindings: 2, Reason: model.ExecutionTargetRecoveryReason, Remedy: model.ExecutionTargetRecoveryRemedy}, nil
}

func TestStatusReportsQuarantinedTargetBindingsWithRemedy(t *testing.T) {
	value, err := (Health{Store: targetRecoveryHealthStore{}}).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.ExecutionTargetRecovery == nil || status.ExecutionTargetRecovery.QuarantinedBindings != 2 {
		t.Fatal(status)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "execution_target_recovery") || !strings.Contains(string(raw), model.ExecutionTargetRecoveryRemedy) {
		t.Fatal("operator remedy missing", string(raw))
	}
}
