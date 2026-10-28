package gitcache

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPSRefusalNeverServesStaleCache(t *testing.T) {
	repository := newTestRepository(t)
	remote := repository.upstream
	cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return remote, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	remote = "https://forge.example/repo.git"
	result, err := cache.Ensure(context.Background(), "example")
	if !errors.Is(err, ErrUpstreamRefused) || result.Stale || !strings.Contains(err.Error(), "built without curl") {
		t.Fatalf("HTTPS served stale or lost refusal: %+v %v", result, err)
	}
	env := cache.commandEnv(map[string]string{"GIT_ALLOW_PROTOCOL": "ssh:https:file"})
	for _, value := range env {
		if value == "GIT_ALLOW_PROTOCOL=ssh:file" {
			return
		}
	}
	t.Fatal("caller re-enabled HTTPS transport")
}
