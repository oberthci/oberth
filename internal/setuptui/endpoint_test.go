package setuptui

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestClusterIPWizardDoesNotProbeNodePorts(t *testing.T) {
	old := dialPort
	defer func() { dialPort = old }()
	calls := 0
	dialPort = func(context.Context, string) error { calls++; return nil }
	state := &WizardState{Config: installer.Config{ServiceType: "ClusterIP"}, ClientAccess: "none"}
	var result verifyResult
	checkClientSide(t.Context(), state, &result)
	if calls != 0 || result.portsChecked {
		t.Fatalf("ClusterIP probed %d node endpoints: %+v", calls, result)
	}
}
