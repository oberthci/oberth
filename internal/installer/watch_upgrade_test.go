package installer

import (
	"strings"
	"testing"
)

func TestUpgradePreservesWatchEnablementAndAllowsExplicitChoice(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"true", "false"} {
		if err := checkPinnedKeyConflicts([]string{"watchTunnel.enabled=" + value}, nil); err != nil {
			t.Fatalf("explicit boolean connector choice rejected: %v", err)
		}
	}
	args := strings.Join(upgradeHelmArgs(UpgradeConfig{}, "chart", "image"), " ")
	if strings.Contains(args, "watchTunnel.enabled=") {
		t.Fatal("upgrade overwrites reused connector enablement instead of preserving administrator choice")
	}
	if !strings.Contains(args, "watchTunnel.image="+watchTunnelImageDefault) || !strings.Contains(args, "watchTunnel.openbaoImage="+watchTunnelOpenbaoImageDefault) {
		t.Fatal("preserving enablement weakened immutable image pins")
	}
}
