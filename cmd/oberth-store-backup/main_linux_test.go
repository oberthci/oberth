//go:build linux

package main

import (
	"bytes"
	"testing"
)

func TestBackupRejectsArgumentsWithoutPrivateIO(t *testing.T) {
	for _, args := range [][]string{{}, {"--unknown"}, {"extra"}} {
		var b bytes.Buffer
		if e := run(args, &b); e == nil || b.Len() != 0 {
			t.Fatal("invalid arguments accepted")
		}
	}
}
