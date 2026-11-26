package argojob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func rerunBundle(t *testing.T, files map[string][]byte, removed []string, commit string) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	if err := writeHoldFrame(&out, rerunManifest{RunID: "r2", Commit: commit, Removed: removed}); err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(out.Bytes())
	return out.Bytes(), hex.EncodeToString(h[:])
}

func minimalRerunFiles() map[string][]byte {
	return map[string][]byte{".git/HEAD": []byte(strings.Repeat("f", 40) + "\n"), ".git/index": []byte("index"), "FIXED": []byte("yes")}
}

func rerunRuntime(t *testing.T) holdRuntime {
	t.Helper()
	r, _ := testHoldRuntime(t)
	r.sourceRoot = filepath.Join(holdTempDir(t), "src")
	if err := os.Mkdir(r.sourceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRerunReplacesTrackedKeepsUntracked(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	for name, text := range map[string]string{"changed": "P0", "deleted": "P0", "untracked": "service state"} {
		if err := os.WriteFile(filepath.Join(r.sourceRoot, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(r.sourceRoot, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.sourceRoot, ".git", "stale-ref"), []byte("P0"), 0600); err != nil {
		t.Fatal(err)
	}
	files := minimalRerunFiles()
	files["changed"], files["added"] = []byte("F"), []byte("new")
	bundle, digest := rerunBundle(t, files, []string{"deleted"}, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	commit, err := applyRerunSnapshot(t.Context(), r.sourceRoot, stage, digest)
	if err != nil || commit.Commit != strings.Repeat("f", 40) {
		t.Fatal(commit, err)
	}
	assertRerunFiles(t, r.sourceRoot)
}

func assertRerunFiles(t *testing.T, root string) {
	t.Helper()
	for name, text := range map[string]string{"changed": "F", "added": "new", "untracked": "service state"} {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(body) != text {
			t.Fatal(name, string(body), err)
		}
	}
	for _, name := range []string{"deleted", ".git/stale-ref"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("stale path survived", name, err)
		}
	}
}

func TestRerunCheckoutIsF(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	git, p0, fix := checkoutTestRepository(t, r.sourceRoot)
	files := checkoutTestFiles(t, r.sourceRoot)
	// C6 exports a detached HEAD at the tested commit.
	files[".git/HEAD"] = []byte(fix + "\n")
	bundle, digest := rerunBundle(t, files, nil, fix)
	git("checkout", "-q", "--detach", p0)
	git("tag", "-d", "snapshot-tag")
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	if _, err = applyRerunSnapshot(t.Context(), r.sourceRoot, stage, digest); err != nil {
		t.Fatal(err)
	}
	if got := git("rev-parse", "HEAD"); got != fix {
		t.Fatal("HEAD is not F", got, fix)
	}
	if got := git("status", "--porcelain"); got != "" {
		t.Fatal("index differs from F", got)
	}
	if got := git("rev-parse", "snapshot-tag"); got != fix {
		t.Fatal("F refs were not installed", got)
	}
}

func checkoutTestRepository(t *testing.T, root string) (func(...string) string, string, string) {
	t.Helper()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("P0"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked")
	git("commit", "-qm", "Initial")
	p0 := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("F"), 0644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "Fix")
	fix := git("rev-parse", "HEAD")
	git("tag", "snapshot-tag")
	return git, p0, fix
}

func checkoutTestFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "tracked" || rel == ".git/HEAD" || rel == ".git/index" || strings.HasPrefix(rel, ".git/objects/") || strings.HasPrefix(rel, ".git/refs/") {
			files[rel], err = os.ReadFile(p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRerunRootNotSymlink(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.sourceRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, r.sourceRoot); err != nil {
		t.Fatal(err)
	}
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	o, _, _ := testHoldOwner(t)
	o.runtime.sourceRoot = r.sourceRoot
	_, frame, _, _ := o.admit(holdRequest{Version: holdProtocol, Op: "rerun", UID: "uid", Stage: stage, Digest: digest})
	if frame.Type != "refused" {
		t.Fatal("symlink root admitted", frame)
	}
	_, frame, _, _ = o.admit(holdRequest{Version: holdProtocol, Op: "status", UID: "uid"})
	if frame.Type != "held" {
		t.Fatal("refusal closed hold", frame)
	}
	if _, err = applyRerunSnapshot(t.Context(), r.sourceRoot, stage, digest); err == nil {
		t.Fatal("symlink root installed")
	}
	if _, err = os.Stat(filepath.Join(outside, "FIXED")); !os.IsNotExist(err) {
		t.Fatal("escaped root", err)
	}
}

func TestHoldRerunOneShot(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	o, _, _ := testHoldOwner(t)
	o.runtime.sourceRoot = r.sourceRoot
	request := holdRequest{Version: holdProtocol, Op: "rerun", UID: "uid", Stage: stage, Digest: digest}
	_, frame, _, end := o.admit(request)
	if frame.Type != "rerunning" || !end {
		t.Fatal(frame, end)
	}
	_, frame, _, _ = o.admit(request)
	if frame.Type != "already_rerun" {
		t.Fatal("rerun repeated", frame)
	}
	_, frame, _, _ = o.admit(holdRequest{Version: holdProtocol, Op: "exec", UID: "uid", Argv: []string{"true"}})
	if frame.Type != "closed" {
		t.Fatal("inspection after rerun", frame)
	}
	_, frame, _, _ = o.admit(holdRequest{Version: holdProtocol, Op: "release", UID: "uid"})
	if frame.Type != "closed" {
		t.Fatal(frame)
	}
}

func TestHoldRerunRacesClose(t *testing.T) {
	t.Parallel()
	for range 30 {
		r := rerunRuntime(t)
		bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
		stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
		if err != nil {
			t.Fatal(err)
		}
		o, _, _ := testHoldOwner(t)
		o.runtime.sourceRoot = r.sourceRoot
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			o.admit(holdRequest{Version: holdProtocol, Op: "rerun", UID: "uid", Stage: stage, Digest: digest})
		}()
		go func() { defer wg.Done(); o.close() }()
		wg.Wait()
		o.mu.Lock()
		if !o.closing {
			t.Error("no transition won")
		}
		o.mu.Unlock()
		_ = os.Remove(stage)
	}
}

func TestHoldRerunReturnsAuthoredExit(t *testing.T) {
	for _, exit := range []int{0, 17, 128 + int(syscall.SIGTERM)} {
		t.Run(string(rune('a'+exit)), func(t *testing.T) {
			r := rerunRuntime(t)
			diagnostic := &holdReadyWriter{ready: make(chan struct{})}
			finished := make(chan int, 1)
			command := "test -f \"$1/FIXED\" || exit 9; exit \"$2\""
			if exit > 128 {
				command = "test -f \"$1/FIXED\" || exit 9; kill -TERM $$"
			}
			go func() {
				finished <- r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", command, "sh", r.sourceRoot, fmt.Sprint(exit)}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: diagnostic})
			}()
			select {
			case <-diagnostic.ready:
			case <-time.After(3 * time.Second):
				t.Fatal("hold did not start")
			}
			bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
			var response bytes.Buffer
			code := r.client(t.Context(), []string{"rerun", "--pod-uid", "uid", "--sha256", digest}, HoldIO{In: bytes.NewReader(bundle), Out: io.Discard, Err: &response})
			if code != 0 {
				t.Fatal(code, response.String())
			}
			select {
			case got := <-finished:
				if got != exit {
					t.Fatal("authored exit changed", got, exit)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("rerun did not finish")
			}
			if got := r.dispatch(context.Background(), []string{"run", "--", "true"}, HoldIO{Out: io.Discard, Err: io.Discard}); got != 70 {
				t.Fatal("restart reran command", got)
			}
		})
	}
}
