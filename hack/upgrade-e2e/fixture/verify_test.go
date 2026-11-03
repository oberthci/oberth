package main

import "testing"

func TestMigrationUpgradeAndRestart(t *testing.T) {
	const now int64 = 1800000000000000000
	old := snapshotState{Ledger: []migration{{1, now - 10000}}, Runs: []run{{ID: "durable", Sequence: 1}}, Total: 1}
	upgraded := old
	upgraded.Ledger = []migration{{1, now - 10000}, {2, now + 1}}
	if err := compareState(old, upgraded, 2, now); err != nil {
		t.Fatal(err)
	}
	if err := compareState(upgraded, upgraded, 2, now+100); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		ledger   []migration
		expected int
	}{
		{"missing", old.Ledger, 2},
		{"duplicate", []migration{{1, now - 10000}, {1, now + 1}}, 2},
		{"gap", []migration{{1, now - 10000}, {3, now + 1}}, 2},
		{"predates upgrade", []migration{{1, now - 10000}, {2, now - 1}}, 2},
		{"one nanosecond rewrite", []migration{{1, now - 9999}, {2, now + 1}}, 2},
		{"unexpected extra", upgraded.Ledger, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := old
			after.Ledger = tc.ledger
			if err := compareState(old, after, tc.expected, now); err == nil {
				t.Fatal("accepted invalid ledger")
			}
		})
	}
	replayed := upgraded
	replayed.Ledger = []migration{{1, now - 10000}, {2, now + 2}}
	if err := compareState(upgraded, replayed, 2, now+100); err == nil {
		t.Fatal("accepted replay on restart")
	}
	changed := upgraded
	changed.Total++
	if err := compareState(old, changed, 2, now); err == nil {
		t.Fatal("accepted run drift")
	}
}
