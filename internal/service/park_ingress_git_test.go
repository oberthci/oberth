package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

// Losing the transport immediately after Ensure must not force a second
// fetch under the policy deadline. Mirroring uses the restored upstream.
type parkPolicyTransportCut struct {
	*gitcache.Cache
	upstream       string
	fixtureContext context.Context
}

// Keep the service's policy mode values, but give real Git fixture plumbing
// the test's bounded lifetime. Decoder/transport assertions must not depend on
// finishing subprocesses within the production two-second fallback window.
// TestParkPushPolicyDeadlineFallsBackToCI exercises that real window separately.
type parkPolicyFixtureContext struct {
	context.Context
	policyContext context.Context
}

func (c parkPolicyFixtureContext) Value(key any) any {
	return c.policyContext.Value(key)
}

func (g *parkPolicyTransportCut) ReadDefaultBlob(ctx context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	offline := g.upstream + ".offline"
	if err := os.Rename(g.upstream, offline); err != nil {
		return "", nil, false, err
	}
	sha, body, exists, err := g.Cache.ReadDefaultBlob(parkPolicyFixtureContext{Context: g.fixtureContext, policyContext: ctx}, repo, file, limit)
	return sha, body, exists, errors.Join(err, os.Rename(offline, g.upstream))
}

func TestParkPushReusesVerifiedUpstreamPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		park         bool
	}{
		{name: "absent policy defaults", park: true},
		{name: "configured policy", policy: "allow_without_branch_ci: false\n", park: true},
		{name: "invalid policy", policy: "allow_without_branch_ci: invalid\n"},
		{name: "required branch CI", policy: "require_branch_ci: ['park/*']\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newControlFixture(t)
			root := t.TempDir()
			work, upstream := filepath.Join(root, "work"), filepath.Join(root, "upstream.git")
			if err := os.Mkdir(work, 0o700); err != nil {
				t.Fatal(err)
			}
			git := func(dir string, args ...string) string {
				t.Helper()
				command := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
				command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git(work, "init", "--initial-branch=main")
			if tc.policy != "" {
				if err := os.Mkdir(filepath.Join(work, ".oberth"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(work, ".oberth", "promotion.yaml"), []byte(tc.policy), 0o600); err != nil {
					t.Fatal(err)
				}
				git(work, "add", ".oberth/promotion.yaml")
			}
			git(work, "commit", "--allow-empty", "-m", "upstream policy")
			git(work, "clone", "--bare", work, upstream)
			cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(root, "cache"), AllowLocalUpstreams: true, Upstream: func(string) (string, error) { return upstream, nil }})
			if err != nil {
				t.Fatal(err)
			}
			ready, err := cache.Ensure(t.Context(), f.repo.Name)
			if err != nil {
				t.Fatal(err)
			}
			git(work, "commit", "--allow-empty", "-m", "park candidate")
			sha := git(work, "rev-parse", "HEAD")
			git(work, "push", ready.Path, "HEAD:refs/heads/park/review")
			fixtureContext, cancelFixture := context.WithTimeout(t.Context(), time.Minute)
			defer cancelFixture()
			g := &parkPolicyTransportCut{Cache: cache, upstream: upstream, fixtureContext: fixtureContext}
			p, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: f.store, Branches: f.scheduler, Auditor: f.store})
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.Branch(t.Context(), BranchPush{EventID: "park", Repository: f.repo.Name, Branch: "park/review", NewOID: sha, Actor: "dev"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.park {
				if result.Run != nil || git(work, "--git-dir="+upstream, "rev-parse", "refs/heads/park/review") != sha {
					t.Fatalf("park was not mirrored without a run: %+v", result)
				}
			} else if result.Run == nil {
				t.Fatalf("required CI was skipped: %+v", result)
			} else if tc.name == "invalid policy" && !strings.Contains(result.Run.Reason, "invalid promotion policy") {
				t.Fatalf("invalid policy fallback is invisible: %+v", result.Run)
			}
		})
	}
}
