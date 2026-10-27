package gitcache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	if len(args) != 5 {
		os.Exit(2)
	}
	switch args[0] {
	case "real":
		if err := RunShadow(context.Background(), args[1], args[2], args[3], args[4], os.Stdout); err != nil {
			os.Exit(1)
		}
	case "mismatch", "tree-mismatch":
		result := shadowComparison{args[2], strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 40), strings.Repeat("c", 40)}
		if args[0] == "tree-mismatch" {
			result.GoArchive, result.GoTree = result.CLIArchive, strings.Repeat("d", 40)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case "timeout":
		<-time.After(time.Hour) // Deliberately hung workload, killed by its parent.
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
	for _, mode := range []string{"real", "mismatch", "tree-mismatch", "timeout", "crash"} {
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
			cache.timeout = time.Second
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
				t.Fatalf("archive/checkout coverage: %+v", stats)
			}
			if mode == "mismatch" || mode == "tree-mismatch" {
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
			} else if mode == "timeout" || mode == "crash" {
				if stats.Errors != 2 || stats.Mismatches != 0 {
					t.Fatal(stats)
				}
				if mode == "timeout" && !strings.Contains(logs.String(), "reason=timeout") {
					t.Fatal(logs.String())
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
