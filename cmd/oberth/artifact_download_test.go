package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

// artifactFixture is the production service and HTTP handler over a real
// artifact store, served on loopback so the REST client, `oberth artifacts`
// and `oberth mcp call --output` all exercise the real transport (#1011).
type artifactFixture struct {
	runID  string
	root   string
	files  *artifacts.Store
	server *httptest.Server
}

func newArtifactFixture(t *testing.T) artifactFixture {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(root, "db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream, err := db.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "origin", Kind: "forgejo", BaseURL: "https://forge.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Trigger: "branch", Actor: "test@host"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := artifacts.Open(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := service.NewAPI(service.APIConfig{Runs: db, History: db, Repositories: db, Artifacts: files})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler.Handler())
	t.Cleanup(server.Close)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "test-token")
	t.Setenv("OBERTH_TOKEN_COMMAND", "")
	t.Setenv("OBERTH_CA_CERT", "")
	t.Setenv("OBERTH_MCP_URL", server.URL+"/mcp")
	original := mcpHTTPClientFunc
	mcpHTTPClientFunc = func(string) (*http.Client, error) { return server.Client(), nil }
	t.Cleanup(func() { mcpHTTPClientFunc = original })
	return artifactFixture{runID: run.ID, root: root, files: files, server: server}
}

// writeBinary stores size deterministic pseudo-random bytes, which contain
// NULs and invalid UTF-8 throughout, and returns their SHA-256.
func (fixture artifactFixture) writeBinary(t *testing.T, name string, size int64) string {
	t.Helper()
	path := filepath.Join(fixture.root, "artifacts", fixture.runID, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path) //nolint:gosec // test-owned temporary path
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	source := rand.NewChaCha8([32]byte{byte(size), byte(size >> 8), byte(size >> 16), byte(size >> 24)})
	if _, err := io.CopyN(io.MultiWriter(file, hash), source, size); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func fileDigest(t *testing.T, path string) (int64, string) {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // test-owned temporary path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		t.Fatal(err)
	}
	return size, hex.EncodeToString(hash.Sum(nil))
}

type hashingWriter struct {
	size int64
	hash interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func newHashingWriter() *hashingWriter { return &hashingWriter{hash: sha256.New()} }

func (writer *hashingWriter) Write(p []byte) (int, error) {
	writer.size += int64(len(p))
	return writer.hash.Write(p)
}

func (writer *hashingWriter) digest() string { return hex.EncodeToString(writer.hash.Sum(nil)) }

func TestBinaryArtifactsRoundTripByteIdenticalOverHTTPCLIAndMCP(t *testing.T) {
	fixture := newArtifactFixture(t)
	for _, size := range []int64{5 << 20, 64 << 20} {
		name := fmt.Sprintf("build/tool-%dMiB", size>>20)
		want := fixture.writeBinary(t, name, size)
		t.Run(strconv.FormatInt(size>>20, 10)+"MiB", func(t *testing.T) {
			// Raw HTTP: full body, exact Content-Length and declared digest.
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				fixture.server.URL+"/api/runs/"+fixture.runID+"/artifacts/"+name, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer test-token")
			response, err := fixture.server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			received := newHashingWriter()
			_, copyErr := io.Copy(received, response.Body)
			_ = response.Body.Close()
			if copyErr != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("HTTP download: status %d, err %v", response.StatusCode, copyErr)
			}
			if response.ContentLength != size || received.size != size || received.digest() != want {
				t.Fatalf("HTTP: Content-Length %d, received %d bytes sha256 %s; want %d bytes sha256 %s",
					response.ContentLength, received.size, received.digest(), size, want)
			}
			if got := response.Header.Get(api.ArtifactSHA256Header); got != want {
				t.Fatalf("HTTP %s = %q, want %s", api.ArtifactSHA256Header, got, want)
			}

			// CLI to stdout.
			stdout := newHashingWriter()
			if err := runArtifacts(t.Context(), []string{fixture.runID, name}, stdout); err != nil {
				t.Fatalf("oberth artifacts to stdout: %v", err)
			}
			if stdout.size != size || stdout.digest() != want {
				t.Fatalf("CLI stdout: %d bytes sha256 %s", stdout.size, stdout.digest())
			}

			// CLI save-to-file.
			saved := filepath.Join(t.TempDir(), "cli.bin")
			if err := runArtifacts(t.Context(), []string{"--output", saved, fixture.runID, name}, io.Discard); err != nil {
				t.Fatalf("oberth artifacts --output: %v", err)
			}
			if gotSize, got := fileDigest(t, saved); gotSize != size || got != want {
				t.Fatalf("CLI file: %d bytes sha256 %s", gotSize, got)
			}

			// MCP save-to-file.
			mcpSaved := filepath.Join(t.TempDir(), "mcp.bin")
			var summary bytes.Buffer
			arguments := fmt.Sprintf(`{"id":%q,"name":%q}`, fixture.runID, name)
			if err := runMCPCall(t.Context(), []string{"--output", mcpSaved, "artifact_get", arguments}, &summary); err != nil {
				t.Fatalf("oberth mcp call artifact_get --output: %v", err)
			}
			if gotSize, got := fileDigest(t, mcpSaved); gotSize != size || got != want {
				t.Fatalf("MCP file: %d bytes sha256 %s", gotSize, got)
			}
			var report mcpArtifactSaved
			if err := json.Unmarshal(summary.Bytes(), &report); err != nil || !report.Verified || report.SHA256 != want || report.Bytes != size {
				t.Fatalf("MCP summary %s (%v)", summary.String(), err)
			}
		})
	}
}

func TestMCPInlineArtifactRefusesBinaryAndKeepsTextBounds(t *testing.T) {
	fixture := newArtifactFixture(t)
	want := fixture.writeBinary(t, "build/tool", 5<<20)
	textPath := filepath.Join(fixture.root, "artifacts", fixture.runID, "report.txt")
	text := strings.Repeat("line of a large text report\n", (5<<20)/28)
	if err := os.WriteFile(textPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	call := func(name string) (bool, string) {
		t.Helper()
		var out bytes.Buffer
		err := runMCPCall(t.Context(), []string{"artifact_get", fmt.Sprintf(`{"id":%q,"name":%q}`, fixture.runID, name)}, &out)
		if err != nil && !errors.Is(err, errMCPToolError) {
			t.Fatal(err)
		}
		return errors.Is(err, errMCPToolError), out.String()
	}
	// A binary never comes back as a partial or mangled inline body.
	isError, message := call("build/tool")
	if !isError || !strings.Contains(message, "binary") || !strings.Contains(message, want) || !strings.Contains(message, "--output") {
		t.Fatalf("inline binary: isError=%v %q", isError, message)
	}
	// Text keeps the bounded, explicitly flagged inline view, named by the
	// digest of the whole object.
	isError, body := call("report.txt")
	var view struct {
		Truncated bool   `json:"truncated"`
		Bytes     int64  `json:"bytes"`
		SHA256    string `json:"sha256"`
	}
	if isError || json.Unmarshal([]byte(body), &view) != nil {
		t.Fatalf("inline text: isError=%v %.200q", isError, body)
	}
	sum := sha256.Sum256([]byte(text))
	if !view.Truncated || view.Bytes != int64(len(text)) || view.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("inline text view: truncated=%v bytes=%d sha256=%s", view.Truncated, view.Bytes, view.SHA256)
	}
}

func TestOversizedArtifactCollectionStoresNothingToDownload(t *testing.T) {
	fixture := newArtifactFixture(t)
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	body := bytes.Repeat([]byte{0, 0xff, 'x'}, (5<<20)/3+1)
	for _, member := range []struct {
		name string
		body []byte
	}{{"small.txt", []byte("ok\n")}, {"build/huge", body}} {
		if err := writer.WriteHeader(&tar.Header{Name: member.name, Mode: 0o644, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	if _, err := zipper.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.files.Extract(fixture.runID, &compressed, 4<<20, artifacts.DefaultScanPatterns); !errors.Is(err, artifacts.ErrTooLarge) {
		t.Fatalf("Extract over the per-run limit: %v", err)
	}
	for _, name := range []string{"build/huge", "small.txt"} {
		saved := filepath.Join(t.TempDir(), "out.bin")
		err := runArtifacts(t.Context(), []string{"--output", saved, fixture.runID, name}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("%s: a refused collection was downloadable: %v", name, err)
		}
		assertNoFiles(t, filepath.Dir(saved))
	}
}

func assertNoFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed download left %d file(s) behind, first %q", len(entries), entries[0].Name())
	}
}

// tamperedArtifactServer serves body under a declared digest and length.
func tamperedArtifactServer(t *testing.T, body []byte, digest string, length int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/artifacts/") {
			_, _ = w.Write([]byte(`[{"ID":"0123456789abcdef0123456789abcdef"}]`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(length))
		if digest != "" {
			w.Header().Set(client.ArtifactSHA256Header, digest)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		if len(body) < length {
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "test-token")
	t.Setenv("OBERTH_TOKEN_COMMAND", "")
	t.Setenv("OBERTH_CA_CERT", "")
	t.Setenv("OBERTH_MCP_URL", server.URL+"/mcp")
	original := mcpHTTPClientFunc
	mcpHTTPClientFunc = func(string) (*http.Client, error) { return server.Client(), nil }
	t.Cleanup(func() { mcpHTTPClientFunc = original })
	return server
}

func TestArtifactDownloadsFailOnDigestMismatchOrShortBody(t *testing.T) {
	body := bytes.Repeat([]byte{0x7f, 0, 0xfe}, 1<<20)
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64)
	const runID = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, digest string
		length       int
	}{
		{"digest mismatch", wrong, len(body)},
		{"short body", good, len(body) + 4096},
		{"malformed digest", "not-a-digest", len(body)},
		{"no digest", "", len(body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tamperedArtifactServer(t, body, tc.digest, tc.length)
			directory := t.TempDir()
			cli := runArtifacts(t.Context(), []string{"--output", filepath.Join(directory, "cli.bin"), runID, "build/tool"}, io.Discard)
			mcp := runMCPCall(t.Context(), []string{"--output", filepath.Join(directory, "mcp.bin"), "artifact_get",
				fmt.Sprintf(`{"id":%q,"name":"build/tool"}`, runID)}, io.Discard)
			for label, err := range map[string]error{"CLI": cli, "MCP": mcp} {
				if err == nil {
					t.Fatalf("%s saved a %s download", label, tc.name)
				}
				if tc.digest != "" && !errors.Is(err, client.ErrArtifactIntegrity) {
					t.Fatalf("%s: %v is not an integrity failure", label, err)
				}
			}
			assertNoFiles(t, directory)
			stdoutErr := runArtifacts(t.Context(), []string{runID, "build/tool"}, io.Discard)
			if tc.digest == "" {
				// An old server: stdout keeps working with a warning.
				if stdoutErr != nil {
					t.Fatalf("stdout from a digest-less server: %v", stdoutErr)
				}
			} else if !errors.Is(stdoutErr, client.ErrArtifactIntegrity) {
				t.Fatalf("stdout %s exited cleanly: %v", tc.name, stdoutErr)
			}
		})
	}
}

func TestArtifactDigestHeaderNameIsSharedByServerAndClient(t *testing.T) {
	if api.ArtifactSHA256Header != client.ArtifactSHA256Header {
		t.Fatalf("server sends %q, client verifies %q", api.ArtifactSHA256Header, client.ArtifactSHA256Header)
	}
}

func TestMCPArtifactOutputRefusesFiltersAndOtherTools(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "test-token")
	directory := t.TempDir()
	for _, arguments := range [][]string{
		{"--output", filepath.Join(directory, "x"), "status", `{}`},
		{"--output", filepath.Join(directory, "x"), "artifact_get", `{"id":"abc","name":"a","pattern":"x"}`},
		{"--output", filepath.Join(directory, "x"), "artifact_get", `{"id":"../x","name":"a"}`},
	} {
		if err := runMCPCall(context.Background(), append([]string{"--url", "https://127.0.0.1:1/mcp"}, arguments...), io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v", arguments, err)
		}
	}
	assertNoFiles(t, directory)
}
