package gitcache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

func shadowTestCommand(t *testing.T, mode string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{executable, "-test.run=^TestShadowHelperProcess$", "--", mode}
}

func TestShadowHelperProcess(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]

	if len(args) > 0 && strings.HasPrefix(args[0], "ready-timeout:") {
		connection, err := net.DialTimeout("tcp", strings.TrimPrefix(args[0], "ready-timeout:"), 5*time.Second)
		if err != nil {
			os.Exit(2)
		}
		_ = connection.Close()
		<-time.After(time.Hour)
	}
	if len(args) != 5 {
		os.Exit(2)
	}
	switch args[0] {
	case "real":
		os.Exit(RunShadowHelper(context.Background(), args[1:], os.Stdout))
	case "mismatch", "tree-mismatch", "tree-mismatch-attr":
		result := shadowComparison{SHA: args[2], CLIArchive: strings.Repeat("a", 64), GoArchive: strings.Repeat("b", 64), CLITree: strings.Repeat("c", 40), GoTree: strings.Repeat("c", 40)}
		if args[0] == "tree-mismatch" {
			result.GoArchive = result.CLIArchive
		}
		if args[0] == "tree-mismatch" || args[0] == "tree-mismatch-attr" {
			result.GoTree = strings.Repeat("d", 40)
		}
		result.ExpectedAttrGap = args[0] == "tree-mismatch-attr"
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case "timeout":
		<-time.After(time.Hour) // Deliberately hung workload, killed by its parent.
	case "nonzero":
		os.Exit(17) // No panic or stderr; still an incomplete observation.
	case "cli-failed":
		os.Exit(shadowExitCLI)
	case "oversize":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("private-source-contents credential=secret", 4096))
		<-time.After(time.Hour) // Reaping must not wait for the normal timeout.
	case "hash-guard":
		blob := &plumbing.MemoryObject{}
		blob.SetType(plumbing.BlobObject)
		_, _ = blob.Write([]byte("corrupt! bytes"))
		blob.SetSize(int64(len("expected bytes")))
		checked := verifiedObject{EncodedObject: blob, expected: plumbing.ComputeHash(plumbing.BlobObject, []byte("expected bytes"))}
		reader, err := checked.Reader()
		if err == nil {
			_, err = io.Copy(io.Discard, reader)
			_ = reader.Close()
		}
		os.Exit(shadowExitCode(err))
	case "priority":
		if err := prepareShadowHelper(); err != nil || !shadowHelperPriorityOK() {
			os.Exit(shadowExitGoOther)
		}
		_ = json.NewEncoder(os.Stdout).Encode(shadowComparison{SHA: args[2], CLIArchive: strings.Repeat("a", 64), GoArchive: strings.Repeat("a", 64), CLITree: strings.Repeat("b", 40), GoTree: strings.Repeat("b", 40)})
	case "canceled":
		os.Exit(shadowExitCanceled)

	case "crash":
		panic("private-source-contents credential=secret")
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

type shadowTestLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *shadowTestLog) Write(body []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(body)
}
func (l *shadowTestLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitTestShadow(t *testing.T, cache *Cache) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := cache.WaitShadow(ctx); err != nil {
		t.Fatalf("shadow did not finish: %v stats=%+v", err, cache.ShadowStats())
	}
}

func TestShadowNeverChangesCLIDelivery(t *testing.T) {
	for _, mode := range []string{"real", "mismatch", "tree-mismatch", "tree-mismatch-attr", "timeout", "crash", "nonzero", "cli-failed", "oversize", "hash-guard", "canceled", "priority"} {
		t.Run(mode, func(t *testing.T) {
			repository := newTestRepository(t)
			var logs shadowTestLog
			cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: BackendGoGit, Shadow: true, ShadowCommand: shadowTestCommand(t, mode), Upstream: func(string) (string, error) { return repository.upstream, nil }, Logger: log.New(&logs, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cache.CloseShadow)
			if cache.backendName != BackendCLI {
				t.Fatal("shadow selected go-git for production delivery")
			}
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			if mode == "timeout" {
				cache.timeout = time.Second
			}
			sha := runGit(t, repository.work, "rev-parse", "HEAD")
			var expected bytes.Buffer
			if err := cache.run(t.Context(), commandSpec{dir: filepath.Join(cache.root, "example.git"), args: []string{"archive", "--format=tar", sha}, stdout: &expected}); err != nil {
				t.Fatal(err)
			}
			var actual bytes.Buffer
			if err := cache.ArchiveTar(t.Context(), "example", "main", &actual); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual.Bytes(), expected.Bytes()) {
				t.Fatal("observer changed CLI archive bytes")
			}
			waitTestShadow(t, cache)
			workspace := filepath.Join(t.TempDir(), "work")
			if err := cache.Checkout(t.Context(), "example", sha, workspace); err != nil {
				t.Fatal(err)
			}
			waitTestShadow(t, cache)
			stats := cache.ShadowStats()
			if stats.Runs != 2 || stats.Skipped != 0 {
				t.Fatalf("archive observations from archive/checkout hooks: %+v", stats)
			}
			if mode == "mismatch" || mode == "tree-mismatch" || mode == "tree-mismatch-attr" {
				if stats.Mismatches != 2 || stats.Errors != 0 {
					t.Fatal(stats)
				}
				var metric bytes.Buffer
				cache.WriteShadowMetrics(&metric)
				if !strings.Contains(metric.String(), "oberth_gitcache_shadow_mismatch_total 2\n") {
					t.Fatal(metric.String())
				}
				if !strings.Contains(logs.String(), `"event":"gitcache_shadow_mismatch"`) || !strings.Contains(logs.String(), `"sha":"`+sha+`"`) || !strings.Contains(logs.String(), `"repo":"example"`) {
					t.Fatal(logs.String())
				}
			} else if mode != "real" && mode != "priority" {
				if stats.Errors != 2 || stats.Mismatches != 0 {
					t.Fatal(stats)
				}

				reason := map[string]string{"timeout": "timeout", "crash": "gogit_other", "nonzero": "gogit_other", "cli-failed": "cli_failed", "oversize": "oversize", "hash-guard": "gogit_hash_guard", "canceled": "canceled"}[mode]
				if !strings.Contains(logs.String(), "reason="+reason) {
					t.Fatal(logs.String())
				}
				var metrics bytes.Buffer
				cache.WriteShadowMetrics(&metrics)
				if !strings.Contains(metrics.String(), fmt.Sprintf("oberth_gitcache_shadow_errors_total{reason=%q} 2\n", reason)) {
					t.Fatal(metrics.String())
				}
			} else if stats.Mismatches != 0 || stats.Errors != 0 {
				t.Fatalf("real archive mismatch: %+v %s", stats, logs.String())
			}
			if strings.Contains(logs.String(), "private-source-contents") || strings.Contains(logs.String(), "credential=secret") {
				t.Fatal("helper stderr leaked")
			}
		})
	}
}

func TestShadowQueueIsBoundedAndNonblocking(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cache := &Cache{root: t.TempDir(), shadow: &shadowState{ctx: ctx, jobs: make(chan shadowJob, shadowQueueSize)}}
	for range shadowQueueSize + 5 {
		cache.queueShadow(filepath.Join(cache.root, "repo.git"), strings.Repeat("a", 40))
	}
	if len(cache.shadow.jobs) != shadowQueueSize || cache.ShadowStats().Skipped != 5 {
		t.Fatal(cache.ShadowStats())
	}
}

func TestShadowCanonicalHashIncludesFileBytesAndMetadata(t *testing.T) {
	// This is a byte corruption of equal length: exactly the incident's class.
	var hashes []string
	for _, test := range []struct {
		body string
		mode int64
	}{{"original", 0644}, {"corrupte", 0644}, {"original", 0755}} {
		digest, err := shadowArchiveDigest(func(w io.Writer) error {
			_, err := io.Copy(w, strings.NewReader(shadowTinyTar(test.body, test.mode)))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, digest)
	}
	if hashes[0] == hashes[1] || hashes[0] == hashes[2] {
		t.Fatal("content/mode change escaped comparison")
	}
}

func shadowTinyTar(body string, mode int64) string {
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	_ = w.WriteHeader(&tar.Header{Name: "source", Size: int64(len(body)), Mode: mode})
	_, _ = fmt.Fprint(w, body)
	_ = w.Close()
	return archive.String()
}

func TestNewShadowStateDefaultEntrypoint(t *testing.T) {
	repository := newTestRepository(t)
	cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Shadow: true, ShadowContext: t.Context(), Upstream: func(string) (string, error) { return repository.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.CloseShadow)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.shadow.command) != 2 || cache.shadow.command[0] != exe || cache.shadow.command[1] != "internal-gitcache-shadow" {
		t.Fatal(cache.shadow.command)
	}
	if err := cache.ArchiveTar(t.Context(), "example", "main", io.Discard); err != nil {
		t.Fatal(err)
	}
	waitTestShadow(t, cache)
	if stats := cache.ShadowStats(); stats.Runs != 1 || stats.Errors != 0 || stats.Mismatches != 0 {
		t.Fatal(stats)
	}
}

func TestNewShadowStateShutdownDoesNotCountCancellation(t *testing.T) {
	// A socket acceptance is the helper-ready handshake. No polling sleeps.
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	repository := newTestRepository(t)
	var logs shadowTestLog
	cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repository.upstream, nil }, Logger: log.New(&logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	cache.shadow = newShadowState(cache, t.Context(), shadowTestCommand(t, "ready-timeout:"+listener.Addr().String()))
	t.Cleanup(cache.CloseShadow)
	sha := runGit(t, repository.work, "rev-parse", "HEAD")
	cache.queueShadow(filepath.Join(cache.root, "example.git"), sha)
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	cache.CloseShadow()
	cache.queueShadow(filepath.Join(cache.root, "example.git"), sha)
	stats := cache.ShadowStats()
	if stats.Runs != 0 || stats.Errors != 0 || stats.Mismatches != 0 || stats.Skipped != 0 || strings.Contains(logs.String(), "gitcache_shadow_error") {
		t.Fatalf("shutdown counted: %+v %s", stats, logs.String())
	}
	var metrics bytes.Buffer
	cache.WriteShadowMetrics(&metrics)
	if strings.Contains(metrics.String(), "} 1\n") {
		t.Fatal(metrics.String())
	}
}

func TestShadowOutputBoundThroughIOCopy(t *testing.T) {
	var output shadowOutput
	_, err := io.Copy(&output, struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 4097))})
	if !errors.Is(err, errShadowOversize) || len(output.Bytes()) > 4096 {
		t.Fatalf("output escaped bound: len=%d err=%v", len(output.Bytes()), err)
	}
}

func TestNewShadowStateRealCLIError(t *testing.T) {
	var logs shadowTestLog
	cache, err := New(Config{Root: t.TempDir(), Shadow: true, ShadowContext: t.Context(), Upstream: func(string) (string, error) { return "", errors.New("must not fetch") }, Logger: log.New(&logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.CloseShadow)
	cache.queueShadow(filepath.Join(cache.root, "missing.git"), strings.Repeat("a", 40))
	waitTestShadow(t, cache)
	if stats := cache.ShadowStats(); stats.Runs != 1 || stats.Errors != 1 || stats.Mismatches != 0 {
		t.Fatal(stats)
	}
	if !strings.Contains(logs.String(), "reason=cli_failed") {
		t.Fatal(logs.String())
	}
}

func TestShadowMissingNiceIsCategorized(t *testing.T) {
	// Do not launch an observer at server priority if nice is unavailable.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	s := &shadowState{command: []string{"unused"}, cache: &Cache{gitBinary: git}}
	_, err = s.compare(t.Context(), shadowJob{})
	if classifyShadowError(err) != shadowNiceUnavailable {
		t.Fatalf("missing nice: %v", err)
	}
}
