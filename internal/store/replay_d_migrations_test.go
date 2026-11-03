package store

import "testing"

func TestReplayDMigrationVersions(t *testing.T) {
	for name, versions := range map[string][2]int{
		"pilot":      {pilotRetirementMigrationVersion, 46},
		"capability": {workflowCapabilityMigrationVersion, 47},
		"execution":  {executionRetirementMigrationVersion, 48},
	} {
		if versions[0] != versions[1] {
			t.Errorf("%s migration = %d, want %d", name, versions[0], versions[1])
		}
	}
}
