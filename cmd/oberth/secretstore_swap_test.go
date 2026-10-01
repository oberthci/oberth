package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSwapRiskClassification(t *testing.T) {
	const header = "Filename\tType\tSize\tUsed\tPriority\n"
	const zram = "/dev/zram0 partition 8388604 0 100\n"
	cases := []struct {
		name, entries string
		backing       map[string]string
		unreadable    bool
		risky         bool
	}{
		{name: "no swap"},
		{name: "RAM-only zram", entries: zram, backing: map[string]string{"zram0": "none\n"}},
		{name: "multiple RAM-only devices", entries: zram + "/dev/zram12 partition 1024 5 -2\n", backing: map[string]string{"zram0": "none\n", "zram12": "none\n"}},
		{name: "zram disk backing", entries: zram, backing: map[string]string{"zram0": "/dev/sda2\n"}, risky: true},
		{name: "disk partition", entries: "/dev/sda2 partition 1024 0 -2\n", risky: true},
		{name: "swap file", entries: "/swapfile file 1024 0 -2\n", risky: true},
		{name: "mixed RAM and disk", entries: zram + "/dev/sda2 partition 1024 0 -2\n", backing: map[string]string{"zram0": "none\n"}, risky: true},
		{name: "unknown block device", entries: "/dev/ram0 partition 1024 0 -2\n", backing: map[string]string{"ram0": "none\n"}, risky: true},
		{name: "missing sysfs", entries: zram, risky: true},
		{name: "unreadable sysfs", entries: zram, unreadable: true, risky: true},
		{name: "empty sysfs", entries: zram, backing: map[string]string{"zram0": ""}, risky: true},
		{name: "malformed sysfs", entries: zram, backing: map[string]string{"zram0": "none\n/dev/sda2\n"}, risky: true},
		{name: "unknown sysfs", entries: zram, backing: map[string]string{"zram0": "unknown\n"}, risky: true},
		{name: "noncanonical path", entries: "/dev/../dev/zram0 partition 1024 0 -2\n", backing: map[string]string{"zram0": "none\n"}, risky: true},
		{name: "relative path", entries: "zram0 partition 1024 0 -2\n", backing: map[string]string{"zram0": "none\n"}, risky: true},
		{name: "nonnumeric device", entries: "/dev/zramX partition 1024 0 -2\n", backing: map[string]string{"zramX": "none\n"}, risky: true},
		{name: "noncanonical number", entries: "/dev/zram00 partition 1024 0 -2\n", backing: map[string]string{"zram00": "none\n"}, risky: true},
		{name: "zram-named file", entries: "/dev/zram0 file 1024 0 -2\n", backing: map[string]string{"zram0": "none\n"}, risky: true},
		{name: "malformed proc row", entries: "/dev/zram0 partition\n", backing: map[string]string{"zram0": "none\n"}, risky: true},
		{name: "malformed proc size", entries: "/dev/zram0 partition unknown 0 -2\n", backing: map[string]string{"zram0": "none\n"}, risky: true},
	}
	originalSwap, originalSys, originalStatfs := swapCheckPath, swapSysBlockPath, secretExecFstatfs
	t.Cleanup(func() { swapCheckPath, swapSysBlockPath, secretExecFstatfs = originalSwap, originalSys, originalStatfs })
	secretExecFstatfs = func(int) (int64, error) { return tmpfsMagic, nil }
	// Stop immediately after the swap gate, before credentials or network I/O.
	t.Setenv("VAULT_ADDR", "")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			swapCheckPath, swapSysBlockPath = filepath.Join(dir, "swaps"), filepath.Join(dir, "sys", "block")
			if err := os.WriteFile(swapCheckPath, []byte(header+tc.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			for name, content := range tc.backing {
				path := filepath.Join(swapSysBlockPath, name, "backing_dev")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unreadable {
				// A directory produces a read error even when tests run as root.
				if err := os.MkdirAll(filepath.Join(swapSysBlockPath, "zram0", "backing_dev"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if got := checkSwapActive(swapCheckPath); got != tc.risky {
				t.Errorf("disk risk = %v, want %v", got, tc.risky)
			}
			for _, strict := range []string{"", "1"} {
				t.Setenv(requireSwaplessEnv, strict)
				var stderr bytes.Buffer
				err := runSecretStoreExec(t.Context(), []string{"--dir=" + dir, "--path=oberth/data/release/test", "--", "true"}, io.Discard, &stderr)
				if strict != "" && tc.risky {
					if err == nil || !strings.Contains(err.Error(), "swap is active") {
						t.Errorf("strict error = %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "VAULT_ADDR is not set") {
					t.Errorf("expected to reach post-swap configuration check, strict=%q: %v", strict, err)
				}
				if got := strings.Contains(stderr.String(), "WARNING: swap"); got != (strict == "" && tc.risky) {
					t.Errorf("warning strict=%q = %q, risky=%v", strict, stderr.String(), tc.risky)
				}
			}
		})
	}
}
