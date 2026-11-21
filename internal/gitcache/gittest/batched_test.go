package gittest

import "testing"

func TestBatchedReceiveStartsAtGit251(t *testing.T) {
	for _, tc := range []struct {
		version string
		batched bool
	}{
		{"git version 2.47.3", false},
		{"git version 2.50.1", false},
		{"git version 2.51.0", true},
		{"git version 2.56.0", true},
		{"git version 2.53.0.windows.1", true},
		{"git version 3.0.0", true},
		{"git version 2", false},
		{"version 2.56.0", false},
		{"", false},
	} {
		if got := batchedReceive(tc.version); got != tc.batched {
			t.Fatal("wrong batched receive decision", tc.version, got)
		}
	}
}
