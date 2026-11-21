package gitcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefWriterOwnsAbsentPackedReplacementFilesOnly(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"update-ref", "gc"} {
		t.Run(operation, func(t *testing.T) { checkPackedCommandOwnership(t, operation) })
	}
}
func checkPackedCommandOwnership(t *testing.T, operation string) {
	t.Helper()

	root := t.TempDir()
	s := command{dir: root, args: []string{operation, "--stdin"},
		refWrites: []string{"refs/heads/topic"}, refDeletes: true,
		input: strings.NewReader("delete refs/heads/topic\n")}
	owned := commandResources(s)
	files := packedRefFiles(root)
	for _, p := range files {
		if err := os.WriteFile(p, []byte("owned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := owned.clean(); err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("owned packed file remains", p, err)
		}
		if err := os.WriteFile(p, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := commandResources(s).clean(); err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("pre-existing packed file was removed", p, err)
		}
	}
}

func TestReceiveCleanupOwnsAbsentPackedFilesAfterPrepared(t *testing.T) {
	t.Parallel()
	for _, existed := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "pre-existing"}[existed], func(t *testing.T) {
			checkPackedReceiveOwnership(t, existed)
		})
	}
}
func checkPackedReceiveOwnership(t *testing.T, existed bool) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "origin", "team", "widget.git")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	files := packedRefFiles(root)
	write := func() {
		for _, p := range files {
			if err := os.WriteFile(p, []byte("packed"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if existed {
		write()
	}
	before := receiveResources(root)
	write()
	record := Reservation{State: "ready", Event: ReceiveEvent{ID: strings.Repeat("a", 32),
		Actor: "actor", Fingerprint: "key", Repository: "origin/team/widget",
		Admission: ReleaseAdmission{DefaultBranch: "refs/heads/main"},
		Updates:   []RefUpdate{{Ref: "refs/heads/topic", OldSHA: strings.Repeat("a", 40)}}}}
	if err := writeReservation(root, record); err != nil {
		t.Fatal(err)
	}
	before.captureReferences(root, record.Event.Updates)
	if err := before.cleanReceive(); err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		_, err := os.Stat(p)
		if existed && err != nil || !existed && !os.IsNotExist(err) {
			t.Fatal("packed file ownership", p, err)
		}
	}
}

func TestExclusiveStartupRemovesAbandonedPackedReplacement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := filepath.Join(root, "repo.git")
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range packedRefFiles(store) {
		if err := os.WriteFile(p, []byte("abandoned"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanMaterializations(root); err != nil {
		t.Fatal(err)
	}
	for _, p := range packedRefFiles(store) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("abandoned packed file remains", p, err)
		}
	}
}

func TestNonDeletingRefWriterNeverOwnsPackedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := command{dir: root, args: []string{"update-ref", "--stdin"}, refWrites: []string{"refs/heads/topic"},
		input: strings.NewReader("update refs/heads/topic " + strings.Repeat("a", 40) + "\n")}
	owned := commandResources(s)
	for _, p := range packedRefFiles(root) {
		if err := os.WriteFile(p, []byte("foreign"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := owned.clean(); err != nil {
		t.Fatal(err)
	}
	for _, p := range packedRefFiles(root) {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("non-deleting command removed a packed file", err)
		}
	}
}

func TestReceiveCleanupKeepsReplacementLock(t *testing.T) {
	store := t.TempDir()
	files := receiveResources(store)
	path := filepath.Join(store, "HEAD.lock")
	if err := os.WriteFile(path, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	files.capture([]string{path})
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := files.cleanReceive(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "another writer" {
		t.Fatal("cleanup removed a replacement lock", string(data), err)
	}
}
