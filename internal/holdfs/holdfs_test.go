package holdfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleDiff = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,4 +1,4 @@
 package main
 
-func answer() int { return 41 }
+func answer() int { return 42 }
 
--- /dev/null
+++ b/NEW.md
@@ -0,0 +1,2 @@
+hello
+world
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
\ No newline at end of file
`

func TestParseAndApplyUnifiedDiff(t *testing.T) {
	patches, err := Parse([]byte(sampleDiff), Limits{MaxBytes: 1 << 20, MaxFiles: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 3 || patches[0].Path != "main.go" || !patches[1].Create || patches[1].Path != "NEW.md" || !patches[2].Delete || patches[2].Path != "gone.txt" {
		t.Fatalf("patches = %+v", patches)
	}
	updated, err := Apply([]byte("package main\n\nfunc answer() int { return 41 }\n\n"), true, patches[0])
	if err != nil || string(updated) != "package main\n\nfunc answer() int { return 42 }\n\n" {
		t.Fatalf("apply = %q, %v", updated, err)
	}
	created, err := Apply(nil, false, patches[1])
	if err != nil || string(created) != "hello\nworld\n" {
		t.Fatalf("create = %q, %v", created, err)
	}
	deleted, err := Apply([]byte("bye"), true, patches[2])
	if err != nil || deleted != nil {
		t.Fatalf("delete = %q, %v", deleted, err)
	}
	if _, err := Apply([]byte("package main\n\nfunc answer() int { return 40 }\n\n"), true, patches[0]); !errors.Is(err, ErrContext) {
		t.Fatalf("mismatched context applied: %v", err)
	}
	if _, err := Apply([]byte("x\n"), true, patches[1]); !errors.Is(err, ErrContext) {
		t.Fatalf("creation over an existing file: %v", err)
	}
	if _, err := Apply(nil, false, patches[0]); !errors.Is(err, ErrContext) {
		t.Fatalf("patch of a missing file: %v", err)
	}
}

func TestParseRefusesUnsafePatches(t *testing.T) {
	cases := map[string]struct {
		diff string
		want error
	}{
		"parent path":     {"--- a/../etc/passwd\n+++ b/../etc/passwd\n@@ -1 +1 @@\n-a\n+b\n", ErrPath},
		"absolute path":   {"--- /etc/passwd\n+++ /etc/passwd\n@@ -1 +1 @@\n-a\n+b\n", ErrPath},
		"git directory":   {"--- a/.git/config\n+++ b/.git/config\n@@ -1 +1 @@\n-a\n+b\n", ErrPath},
		"rename":          {"diff --git a/x b/y\nsimilarity index 100%\nrename from x\nrename to y\n", ErrPath},
		"binary":          {"--- a/x.png\n+++ b/x.png\nBinary files differ\n", ErrFormat},
		"bad counts":      {"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-a\n+b\n", ErrFormat},
		"twice":           {"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-b\n+c\n", ErrFormat},
		"no hunks":        {"--- a/x\n+++ b/x\n", ErrFormat},
		"too many files":  {"--- a/f1\n+++ b/f1\n@@ -1 +1 @@\n-a\n+b\n--- a/f2\n+++ b/f2\n@@ -1 +1 @@\n-a\n+b\n", ErrLimit},
		"too many bytes":  {strings.Repeat(" ", 100), ErrLimit},
		"control in path": {"--- a/x\x01y\n+++ b/x\x01y\n@@ -1 +1 @@\n-a\n+b\n", ErrPath},
	}
	for name, tc := range cases {
		limits := Limits{MaxBytes: 1 << 20, MaxFiles: 8}
		switch name {
		case "too many files":
			limits.MaxFiles = 1
		case "too many bytes":
			limits.MaxBytes = 10
		}
		if _, err := Parse([]byte(tc.diff), limits); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestHelperReadsAndWritesWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "pkg", "escape")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	request, _ := json.Marshal(ReadRequest{Paths: []string{"pkg/a.go", "pkg/missing.go"}, MaxBytes: 1024})
	if code := Run([]string{"read", "--root", root}, bytes.NewReader(request), &stdout, &stderr); code != 0 {
		t.Fatalf("read exit %d: %s", code, stderr.String())
	}
	var read ReadResponse
	if err := json.Unmarshal(stdout.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Files) != 2 || !read.Files[0].Exists || string(read.Files[0].Content) != "package pkg\n" || read.Files[0].SHA256 != Digest([]byte("package pkg\n")) || read.Files[1].Exists {
		t.Fatalf("read = %+v", read)
	}
	for _, escape := range []string{"link/secret", "pkg/escape", "../outside", "/etc/passwd", ".git/config"} {
		stdout.Reset()
		stderr.Reset()
		request, _ := json.Marshal(ReadRequest{Paths: []string{escape}})
		if code := Run([]string{"read", "--root", root}, bytes.NewReader(request), &stdout, &stderr); code == 0 {
			t.Fatalf("read of %q succeeded: %s", escape, stdout.String())
		}
	}
	tiny, _ := json.Marshal(ReadRequest{Paths: []string{"pkg/a.go"}, MaxBytes: 4})
	stdout.Reset()
	if code := Run([]string{"read", "--root", root}, bytes.NewReader(tiny), &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	read = ReadResponse{}
	if err := json.Unmarshal(stdout.Bytes(), &read); err != nil || !read.Files[0].Truncated || read.Files[0].Content != nil || read.Files[0].SHA256 == "" {
		t.Fatalf("oversized read = %+v, %v", read, err)
	}

	write := WriteRequest{Files: []FileWrite{
		{Path: "pkg/a.go", ExpectSHA256: Digest([]byte("package pkg\n")), Content: []byte("package pkg // patched\n")},
		{Path: "new/dir/b.go", ExpectSHA256: "", Content: []byte("package dir\n")},
	}}
	stdout.Reset()
	stderr.Reset()
	body, _ := json.Marshal(write)
	if code := Run([]string{"write", "--root", root}, bytes.NewReader(body), &stdout, &stderr); code != 0 {
		t.Fatalf("write exit %d: %s", code, stderr.String())
	}
	if got, _ := os.ReadFile(filepath.Join(root, "pkg", "a.go")); string(got) != "package pkg // patched\n" {
		t.Fatalf("a.go = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new", "dir", "b.go")); string(got) != "package dir\n" {
		t.Fatalf("b.go = %q", got)
	}
	stale := WriteRequest{Files: []FileWrite{{Path: "pkg/a.go", ExpectSHA256: Digest([]byte("package pkg\n")), Content: []byte("x")}}}
	body, _ = json.Marshal(stale)
	if code := Run([]string{"write", "--root", root}, bytes.NewReader(body), &stdout, &stderr); code == 0 {
		t.Fatal("stale precondition accepted")
	}
	through := WriteRequest{Files: []FileWrite{{Path: "link/secret", ExpectSHA256: Digest([]byte("s3")), Content: []byte("owned")}}}
	body, _ = json.Marshal(through)
	if code := Run([]string{"write", "--root", root}, bytes.NewReader(body), &stdout, &stderr); code == 0 {
		t.Fatal("write through a symlinked directory accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "secret")); string(got) != "s3" {
		t.Fatal("file outside the root was written")
	}
	deletion := WriteRequest{Files: []FileWrite{{Path: "new/dir/b.go", ExpectSHA256: Digest([]byte("package dir\n")), Delete: true}}}
	body, _ = json.Marshal(deletion)
	if code := Run([]string{"write", "--root", root}, bytes.NewReader(body), &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "new", "dir", "b.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deletion did not remove the file")
	}
	release := filepath.Join(t.TempDir(), "before")
	if err := releaseMarker(release); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(release); err != nil {
		t.Fatal("release marker missing")
	}
	for _, path := range []string{release, "relative", "/var/run/argo/ctr/wait/before", ReleaseMarkerPath + "/../../../tmp/x"} {
		if code := Run([]string{"release", "--path", path}, nil, &stdout, &stderr); code == 0 {
			t.Fatalf("release accepted %q; only %s may be created", path, ReleaseMarkerPath)
		}
	}
}
