package gittest

import (
	"strconv"
	"strings"
	"testing"
)

// Git 2.51 is the first release whose receive-pack applies all commands in one
// batched reference transaction, so a push whose every command Git refuses still
// passes the reference-transaction hook. Older Git runs one transaction per
// command and never prepares a refused one. The packaged Git is newer.
const batchedMajor, batchedMinor = 2, 51

// RequireBatchedReceive fails plainly when the fixture Git predates batched
// receive, instead of letting the receive gate report a hook that did not run.
func (f *Fixture) RequireBatchedReceive(t *testing.T) {
	t.Helper()
	version := f.Git(t, f.Root, "version")
	if !batchedReceive(version) {
		t.Fatalf("%s at %s predates batched receive (Git %d.%d); set OBERTH_TEST_GIT to the packaged Git", version, f.Binary, batchedMajor, batchedMinor)
	}
}

func batchedReceive(version string) bool {
	fields := strings.Fields(version)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	return majorErr == nil && minorErr == nil && (major > batchedMajor || major == batchedMajor && minor >= batchedMinor)
}
