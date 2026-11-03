package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
)

// compareState uses int64 timestamps: JSON tools using floating point must not
// round the ledger before comparing an existing migration's application time.
func compareState(before, after snapshotState, expected int, started int64) error {
	if before.Total != after.Total || !reflect.DeepEqual(before.Runs, after.Runs) {
		return fmt.Errorf("durable runs changed")
	}
	if len(after.Ledger) != expected || len(after.Ledger) < len(before.Ledger) {
		return fmt.Errorf("incomplete or regressed migration ledger: got %d, want %d", len(after.Ledger), expected)
	}
	for i, m := range after.Ledger {
		if m.Version != i+1 || m.AppliedAt <= 0 {
			return fmt.Errorf("migration ledger gap, duplicate or invalid timestamp at %d", i+1)
		}
		if i < len(before.Ledger) {
			if m != before.Ledger[i] {
				return fmt.Errorf("migration %d was replayed or rewritten", m.Version)
			}
		} else if m.AppliedAt < started {
			return fmt.Errorf("new migration %d predates upgrade", m.Version)
		}
	}
	return nil
}

func verifyCommand(args []string) {
	if len(args) != 4 {
		panic("usage: fixture verify BEFORE AFTER EXPECTED_VERSION UPGRADE_START_NS")
	}
	read := func(path string) snapshotState {
		data, err := os.ReadFile(path) // #nosec G304,G703 -- The CLI explicitly accepts snapshot input files from its caller.
		must(err)
		var s snapshotState
		must(json.Unmarshal(data, &s))
		return s
	}
	expected, err := strconv.Atoi(args[2])
	must(err)
	started, err := strconv.ParseInt(args[3], 10, 64)
	must(err)
	must(compareState(read(args[0]), read(args[1]), expected, started))
}
