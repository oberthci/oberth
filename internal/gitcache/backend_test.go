package gitcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every existing cache fixture enters through this selector. Process-specific
// tests with an explicit executable continue to exercise the CLI adapter.
func newBackendTestCache(t *testing.T, config Config) (*Cache, error) {
	t.Helper()
	// Native hook fixtures execute this test binary once per ref. Disable only
	// race's one-second successful-exit sleep; keep detection and exit status.
	config.Env = maps.Clone(config.Env)
	if config.Env == nil {
		config.Env = make(map[string]string)
	}
	config.Env["GORACE"] = "atexit_sleep_ms=0"
	return New(config)
}

type recordingBackend struct {
	spec     commandSpec
	capture  bool
	deadline bool
	err      error
}

func (b *recordingBackend) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	b.spec, b.capture = spec, capture
	_, b.deadline = ctx.Deadline()
	return "recorded", b.err
}

func TestBackendRoutesRunAndCapture(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("backend error")
	backend := &recordingBackend{}
	cache := &Cache{backend: backend, timeout: time.Second}
	result, err := cache.capture(t.Context(), "repository", "rev-parse", "HEAD")
	if err != nil || result != "recorded" || !backend.capture || !backend.deadline || backend.spec.dir != "repository" {
		t.Fatalf("capture did not traverse backend: %q %v %#v", result, err, backend)
	}
	backend.err = sentinel
	if err := cache.run(t.Context(), commandSpec{args: []string{"gc", "--auto"}}); !errors.Is(err, sentinel) || backend.capture {
		t.Fatalf("run did not preserve backend error/stream mode: %v", err)
	}
}

// A real Git client invokes this test executable as its server-side helper,
// so protocol/policy fixtures traverse the selected backend as well. Calling
// git receive-pack directly here would silently bypass parity coverage.
type backendServiceFixture struct {
	Config  backendFixtureConfig
	Service Service
}

type backendFixtureConfig struct {
	Root           string
	GitBinary      string
	Env            map[string]string
	Redact         []string
	CommandTimeout time.Duration
}

func backendServiceCommand(t *testing.T, cache *Cache, service Service) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture := backendServiceFixture{Config: backendFixtureConfig{Root: cache.root, GitBinary: cache.gitBinary, Env: cache.env, Redact: cache.redact, CommandTimeout: cache.timeout}, Service: service}
	body, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backend.json")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	return quote(executable) + " -test.run='^TestBackendServiceProcess$' -- oberth-backend-service " + quote(file)
}

func TestBackendServiceProcess(t *testing.T) {
	// A normal suite invocation must never exit or write protocol data.
	marker := -1
	for i, arg := range os.Args {
		if arg == "oberth-backend-service" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	if len(os.Args) != marker+3 {
		fmt.Fprintln(os.Stderr, "invalid backend service fixture arguments")
		os.Exit(2)
	}
	body, err := os.ReadFile(os.Args[marker+1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var fixture backendServiceFixture
	if err := json.Unmarshal(body, &fixture); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cache, err := New(Config{
		Root: fixture.Config.Root, GitBinary: fixture.Config.GitBinary,
		Env: fixture.Config.Env, Redact: fixture.Config.Redact, CommandTimeout: fixture.Config.CommandTimeout,
		Upstream: func(string) (string, error) { return "", errors.New("fixture upstream unused") },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	path := os.Args[marker+2]
	err = cache.serveLocked(context.Background(), path, fixture.Service, "", os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestOnlyProductionBackendIsCLI(t *testing.T) {
	cache, err := New(Config{Root: t.TempDir(), Upstream: func(string) (string, error) { return "unused", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.backend.(*cliBackend); !ok {
		t.Fatalf("non-CLI backend %T", cache.backend)
	}
}
