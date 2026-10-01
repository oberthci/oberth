package goproxy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

// tarEntry describes one entry in the mock tar archive.
type tarEntry struct {
	name     string
	content  []byte
	typeflag byte // tar.TypeReg, tar.TypeSymlink, etc.
}

type mockCache struct {
	tags  []string
	files map[string][]byte // ref:path -> content (for ShowFile)

	// tarEntries, if non-nil, overrides the default tar output.
	tarEntries []tarEntry
}

func (m *mockCache) ListTags(_ context.Context, _ string) ([]string, error) {
	return m.tags, nil
}

func (m *mockCache) TagTime(_ context.Context, _, tag string) (time.Time, error) {
	return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), nil
}

func (m *mockCache) ShowFile(_ context.Context, _, ref, filePath string) ([]byte, error) {
	key := ref + ":" + filePath
	content, ok := m.files[key]
	if !ok {
		return nil, io.EOF
	}
	return content, nil
}

func (m *mockCache) ArchiveTar(_ context.Context, _, ref string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	if m.tarEntries != nil {
		for _, e := range m.tarEntries {
			hdr := &tar.Header{
				Name:     e.name,
				Mode:     0o644,
				Typeflag: e.typeflag,
			}
			if e.typeflag == tar.TypeReg {
				hdr.Size = int64(len(e.content))
			}
			if e.typeflag == tar.TypeSymlink {
				hdr.Linkname = string(e.content)
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if e.typeflag == tar.TypeReg && len(e.content) > 0 {
				if _, err := tw.Write(e.content); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Default: simple go.mod + wire.go (backward compat for existing tests).
	gomod := m.files[ref+":go.mod"]
	if gomod == nil {
		gomod = []byte("module go.example.test/wire\n\ngo 1.22\n")
	}
	if err := tw.WriteHeader(&tar.Header{Name: "go.mod", Size: int64(len(gomod)), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(gomod); err != nil {
		return err
	}
	content := []byte("package wire\n")
	if err := tw.WriteHeader(&tar.Header{Name: "wire.go", Size: int64(len(content)), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(content)
	return err
}

func testHandler() *Handler {
	cache := &mockCache{
		tags: []string{"v0.1.0", "v0.2.0", "v0.3.12", "not-semver"},
		files: map[string][]byte{
			"v0.3.12:go.mod": []byte("module go.example.test/wire\n\ngo 1.22\n"),
		},
	}
	return NewHandler(cache, "go.example.test", map[string]string{"wire": "sample-wire"})
}

func TestList(t *testing.T) {
	handler := testHandler()
	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/list", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "v0.1.0") || !strings.Contains(body, "v0.3.12") {
		t.Fatalf("list should contain semver tags: %s", body)
	}
	if strings.Contains(body, "not-semver") {
		t.Fatalf("list should not contain non-semver tags: %s", body)
	}
}

func TestInfo(t *testing.T) {
	handler := testHandler()
	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.info", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var info map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["Version"] != "v0.3.12" {
		t.Fatalf("unexpected version: %s", info["Version"])
	}
}

func TestMod(t *testing.T) {
	handler := testHandler()
	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.mod", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "module go.example.test/wire") {
		t.Fatalf("unexpected go.mod content: %s", rec.Body.String())
	}
}

// TestModVerbatimBytes verifies that the served .mod response is byte-identical
// to the fixture go.mod, including trailing whitespace. This is the invariant
// that broke in #726 (reopened): tailBuffer.String() applied bytes.TrimSpace,
// stripping the trailing newline from go.mod, which changed its h1: hash and
// caused Go's checksum verification to fail closed with SECURITY ERROR.
func TestModVerbatimBytes(t *testing.T) {
	// Fixture WITH trailing newline (41 bytes — the common case).
	gomod := []byte("module go.example.test/wire\n\ngo 1.25.14\n")
	cache := &mockCache{
		tags: []string{"v0.3.12"},
		files: map[string][]byte{
			"v0.3.12:go.mod": gomod,
		},
	}
	handler := NewHandler(cache, "go.example.test", map[string]string{"wire": "sample-wire"})

	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.mod", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	served := rec.Body.Bytes()
	if !bytes.Equal(served, gomod) {
		t.Fatalf("served .mod not byte-identical to fixture\n  served (%d bytes): %q\n  fixture (%d bytes): %q",
			len(served), served, len(gomod), gomod)
	}
}

// TestModVerbatimNoTrailingNewline verifies that a go.mod WITHOUT a trailing
// newline is served without one — verbatim in both directions.
func TestModVerbatimNoTrailingNewline(t *testing.T) {
	// Fixture WITHOUT trailing newline (40 bytes — unusual but valid).
	gomod := []byte("module go.example.test/wire\n\ngo 1.25.14")
	cache := &mockCache{
		tags: []string{"v0.3.12"},
		files: map[string][]byte{
			"v0.3.12:go.mod": gomod,
		},
	}
	handler := NewHandler(cache, "go.example.test", map[string]string{"wire": "sample-wire"})

	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.mod", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	served := rec.Body.Bytes()
	if !bytes.Equal(served, gomod) {
		t.Fatalf("served .mod not byte-identical to fixture\n  served (%d bytes): %q\n  fixture (%d bytes): %q",
			len(served), served, len(gomod), gomod)
	}
}

// TestModGoSumHash verifies that the h1: hash of the served .mod content
// (computed as dirhash.Hash1 over ["go.mod"] -> content) matches the value
// that would appear in go.sum. This is how `go mod download -json` computes
// the /go.mod h1: line: Hash1 over a single-entry directory containing just
// the go.mod bytes. A mismatch means checksum verification fails (#726).
func TestModGoSumHash(t *testing.T) {
	gomod := []byte("module go.example.test/wire\n\ngo 1.25.14\n")
	cache := &mockCache{
		tags: []string{"v0.3.12"},
		files: map[string][]byte{
			"v0.3.12:go.mod": gomod,
		},
	}
	handler := NewHandler(cache, "go.example.test", map[string]string{"wire": "sample-wire"})

	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.mod", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Compute h1: the same way Go does for /go.mod go.sum lines:
	// dirhash.Hash1 over a virtual directory with one file "go.mod".
	served := rec.Body.Bytes()
	h, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(served)), nil
	})
	if err != nil {
		t.Fatalf("Hash1: %v", err)
	}

	// Reference: compute the expected hash from the fixture bytes directly.
	expected, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(gomod)), nil
	})
	if err != nil {
		t.Fatalf("Hash1 reference: %v", err)
	}

	if h != expected {
		t.Fatalf("go.sum /go.mod h1: mismatch\n  served:   %s\n  expected: %s", h, expected)
	}

	// Pin the known-good vector for this fixture to catch regressions in the
	// fixture itself. The hash of "module go.example.test/wire\n\ngo 1.25.14\n"
	// is stable and deterministic. It is independently derived from SHA256 of
	// the Go directory-hash line: hex(SHA256(go.mod bytes)) + "  go.mod\n".
	const pinnedVector = "h1:m4XEz7yDzz7rCplydC7lr/7bDE4YC2RJ59oZo4agdok="
	if h != pinnedVector {
		t.Fatalf("served .mod h1 does not match pinned vector\n  got:    %s\n  pinned: %s", h, pinnedVector)
	}
}

func TestZip(t *testing.T) {
	handler := testHandler()
	req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.zip", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("unexpected content type: %s", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	foundMod := false
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "go.example.test/wire@v0.3.12/") {
			t.Fatalf("file %q should be prefixed with module@version/", f.Name)
		}
		if f.Name == "go.example.test/wire@v0.3.12/go.mod" {
			foundMod = true
		}
	}
	if !foundMod {
		t.Fatal("zip should contain go.mod")
	}
}

func TestRoutingStatus(t *testing.T) {
	handler := testHandler()
	for _, tt := range []struct {
		path   string
		status int
	}{
		{"/go.example.test/unknown/@v/list", http.StatusForbidden},
		{"/github.com/foo/bar/@v/list", http.StatusNotFound},
		{"/go.example.test/wire/something", http.StatusForbidden},
		{"/go.example.test/wire/@v/notaversion.info", http.StatusForbidden},
	} {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest("GET", tt.path, nil))
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	handler := testHandler()
	req := httptest.NewRequest("POST", "/go.example.test/wire/@v/list", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// TestZipCanonicalHash verifies that the served zip produces the same h1:
// dirhash as golang.org/x/mod/zip.CreateFromDir. This is the invariant that
// broke in #726: the mirror served a zip with non-canonical content (nested
// module files included, raw archive/zip metadata) and Go's checksum
// verification rightly rejected it.
//
// The fixture includes every exclusion-triggering pattern: a nested module
// directory (gen/ with its own go.mod), a vendor/ tree, a symlink entry, and
// a .git-like path. The served zip must match the canonical zip byte-for-byte.
func TestZipCanonicalHash(t *testing.T) {
	gomod := []byte("module go.example.test/testmod\n\ngo 1.22\n")
	cache := &mockCache{
		tags: []string{"v1.0.0"},
		files: map[string][]byte{
			"v1.0.0:go.mod": gomod,
		},
		tarEntries: []tarEntry{
			// Root module files — must be included.
			{name: "go.mod", content: gomod, typeflag: tar.TypeReg},
			{name: "main.go", content: []byte("package testmod\n"), typeflag: tar.TypeReg},
			{name: "sub/helper.go", content: []byte("package sub\n"), typeflag: tar.TypeReg},
			// Nested module (gen/ has its own go.mod) — must be excluded.
			{name: "gen/go.mod", content: []byte("module go.example.test/testmod/gen\n\ngo 1.22\n"), typeflag: tar.TypeReg},
			{name: "gen/go.sum", content: []byte("example.com/dep v0.1.0 h1:abc=\n"), typeflag: tar.TypeReg},
			{name: "gen/main.go", content: []byte("package main\n"), typeflag: tar.TypeReg},
			// Vendor directory — must be excluded.
			{name: "vendor/modules.txt", content: []byte("# vendor\n"), typeflag: tar.TypeReg},
			// Symlink — must not appear (TypeSymlink is not TypeReg, so not
			// extracted to the temp dir; CreateFromDir would also skip it).
			{name: "link.go", content: []byte("main.go"), typeflag: tar.TypeSymlink},
			// .git path — must be excluded by CreateFromDir.
			{name: ".git/config", content: []byte("[core]\n"), typeflag: tar.TypeReg},
		},
	}
	handler := NewHandler(cache, "go.example.test", map[string]string{"testmod": "test-repo"})

	req := httptest.NewRequest("GET", "/go.example.test/testmod/@v/v1.0.0.zip", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	servedBytes := rec.Body.Bytes()

	// Build the reference canonical zip from the same file set, applying
	// the same exclusions that `go mod download` applies: nested modules
	// are excluded by CreateFromDir; vendor/ is excluded before extraction
	// (it is a VCS-layer concern in the Go toolchain, not in CreateFromDir).
	// We include the nested module files so CreateFromDir can detect and
	// skip them, but we omit vendor/ files since our handler skips them
	// during tar extraction (matching Go's behavior).
	refDir := t.TempDir()
	refFiles := []struct{ name, content string }{
		{"go.mod", string(gomod)},
		{"main.go", "package testmod\n"},
		{"sub/helper.go", "package sub\n"},
		// Include the nested module so CreateFromDir detects and skips it.
		{"gen/go.mod", "module go.example.test/testmod/gen\n\ngo 1.22\n"},
		{"gen/go.sum", "example.com/dep v0.1.0 h1:abc=\n"},
		{"gen/main.go", "package main\n"},
		// vendor/ is NOT written: Go's VCS layer excludes it before zip
		// creation, and our handler does the same during tar extraction.
	}
	for _, rf := range refFiles {
		target := filepath.Join(refDir, filepath.FromSlash(rf.name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(rf.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var refBuf bytes.Buffer
	mv := module.Version{Path: "go.example.test/testmod", Version: "v1.0.0"}
	if err := modzip.CreateFromDir(&refBuf, mv, refDir); err != nil {
		t.Fatalf("CreateFromDir: %v", err)
	}

	// Byte-identical comparison: both should produce the exact same output.
	if !bytes.Equal(servedBytes, refBuf.Bytes()) {
		servedHash := hashZipBytes(t, servedBytes)
		refHash := hashZipBytes(t, refBuf.Bytes())
		t.Fatalf("served zip not byte-identical to canonical zip\n  served h1: %s\n  canonical h1: %s", servedHash, refHash)
	}

	// Verify the h1: hash explicitly.
	h := hashZipBytes(t, servedBytes)
	refH := hashZipBytes(t, refBuf.Bytes())
	if h != refH {
		t.Fatalf("h1 hash mismatch: served=%s canonical=%s", h, refH)
	}

	// Verify file inclusion/exclusion in the served zip.
	zr, err := zip.NewReader(bytes.NewReader(servedBytes), int64(len(servedBytes)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}

	prefix := "go.example.test/testmod@v1.0.0/"
	mustInclude := []string{prefix + "go.mod", prefix + "main.go", prefix + "sub/helper.go"}
	for _, want := range mustInclude {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in zip, got: %v", want, names)
		}
	}

	mustExclude := []string{"gen/", "vendor/", "link.go", ".git/"}
	for _, exc := range mustExclude {
		for _, n := range names {
			trimmed := strings.TrimPrefix(n, prefix)
			if strings.HasPrefix(trimmed, exc) || trimmed == exc {
				t.Errorf("zip must not contain %q (excluded pattern %q)", n, exc)
			}
		}
	}
}

// TestZipDeterministic asserts that two requests for the same module version
// produce byte-identical zip output.
func TestZipDeterministic(t *testing.T) {
	handler := testHandler()
	serve := func() []byte {
		req := httptest.NewRequest("GET", "/go.example.test/wire/@v/v0.3.12.zip", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		return rec.Body.Bytes()
	}
	a := serve()
	b := serve()
	if !bytes.Equal(a, b) {
		t.Fatal("two requests for the same version produced different zip bytes")
	}
}

// hashZipBytes writes the zip to a temp file and computes its h1: dirhash.
func hashZipBytes(t *testing.T, zipBytes []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "test.zip")
	if err := os.WriteFile(f, zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := dirhash.HashZip(f, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
