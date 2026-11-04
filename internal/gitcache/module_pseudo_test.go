package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
)

type pseudoSnapshotter interface {
	ModulePseudoVersionSnapshot(context.Context, string, string, string, bool) (string, time.Time, error)
}

func TestModulePseudoVersionReachabilityAndTimestamp(t *testing.T) {
	repo := newTestRepositoryWithBranch(t, "trunk")
	cache := newTestCache(t, repo.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	resolver, ok := any(cache).(pseudoSnapshotter)
	if !ok {
		t.Fatal("pseudo-version snapshots unavailable")
	}
	stamp, err := strconv.ParseInt(runGit(t, repo.work, "show", "-s", "--format=%ct", repo.initialSHA), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Unix(stamp, 0).UTC()
	v := module.PseudoVersion("v0", "", when, repo.initialSHA[:12])
	sha, got, err := resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", v, false)
	if err != nil || sha != repo.initialSHA || !got.Equal(when) {
		t.Fatalf("default source: %s %v %v", sha, got, err)
	}
	for name, invalid := range map[string]string{
		"unknown commit":     module.PseudoVersion("v0", "", when, "ffffffffffff"),
		"tree object":        module.PseudoVersion("v0", "", when, runGit(t, ready.Path, "rev-parse", "HEAD^{tree}")[:12]),
		"short revision":     module.PseudoVersion("v0", "", when, repo.initialSHA[:10]),
		"full revision":      module.PseudoVersion("v0", "", when, repo.initialSHA),
		"uppercase":          module.PseudoVersion("v0", "", when, "ABCDEF012345"),
		"nonhex":             module.PseudoVersion("v0", "", when, "ghijkl012345"),
		"invalid date":       "v0.0.0-20261399000000-" + repo.initialSHA[:12],
		"incompatible major": module.PseudoVersion("v2", "", when, repo.initialSHA[:12]),
		"leading zero":       strings.Replace(v, "v0.", "v00.", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", invalid, true); !errors.Is(err, ErrModulePseudoVersionUnavailable) {
				t.Fatalf("invalid pseudo-version: %v", err)
			}
		})
	}
	bad := module.PseudoVersion("v0", "", when.Add(time.Second), repo.initialSHA[:12])
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", bad, false); err == nil {
		t.Fatal("wrong timestamp accepted")
	}
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T09:08:07+02:00")
	runGit(t, repo.work, "checkout", "-b", "feature/pseudo")
	if err = os.WriteFile(filepath.Join(repo.work, "new.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.work, "add", "new.txt")
	runGit(t, repo.work, "commit", "-m", "feature source")
	feature := runGit(t, repo.work, "rev-parse", "HEAD")
	seedCacheObject(t, repo.work, ready.Path, feature)
	stamp, err = strconv.ParseInt(runGit(t, repo.work, "show", "-s", "--format=%ct", feature), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	fv := module.PseudoVersion("v0", "", time.Unix(stamp, 0), feature[:12])
	runGit(t, ready.Path, "update-ref", "refs/heads/feature/pseudo", feature)
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, false); err == nil {
		t.Fatal("default policy accepted feature branch")
	}
	if sha, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, true); err != nil || sha != feature {
		t.Fatalf("opt-in branch: %s %v", sha, err)
	}
	runGit(t, ready.Path, "update-ref", "-d", "refs/heads/feature/pseudo")
	runGit(t, ready.Path, "update-ref", "refs/oberth/promotions/hidden", feature)
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, true); err == nil {
		t.Fatal("private pin admitted orphan commit")
	}
	// An allowed semantic-version tag can authorize the orphan only after the
	// administrator expands the default-only pseudo-version policy.
	runGit(t, ready.Path, "update-ref", "refs/tags/unversioned", feature)
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, true); err == nil {
		t.Fatal("unversioned tag widened proxy policy")
	}
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.3.0", feature)
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, false); err == nil {
		t.Fatal("tag bypassed default-only policy")
	}
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, true); err != nil {
		t.Fatal("allowed tag ancestry refused", err)
	}
	authorVersion := module.PseudoVersion("v0", "", time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), feature[:12])
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", authorVersion, true); err == nil {
		t.Fatal("author time accepted as committer time")
	}
	// A base-derived pseudo-version needs the exact ancestor tag, and may not
	// pretend to be a later commit than the base tag itself.
	derived := module.PseudoVersion("v1", "v1.2.3", time.Unix(stamp, 0), feature[:12])
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", derived, true); err == nil {
		t.Fatal("nonexistent base tag accepted")
	}
	runGit(t, repo.work, "tag", "-a", "v1.2.3", repo.initialSHA, "-m", "annotated base")
	tagObject := runGit(t, repo.work, "rev-parse", "refs/tags/v1.2.3")
	seedCacheObject(t, repo.work, ready.Path, tagObject)
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.2.3", tagObject)
	if sha, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", derived, true); err != nil || sha != feature {
		t.Fatalf("base-derived version: %s %v", sha, err)
	}
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.2.3", feature)
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", derived, true); err == nil {
		t.Fatal("base tag equals pseudo commit")
	}
	// An existing base on unrelated history cannot lend a version number.
	unrelated := runGit(t, repo.work, "commit-tree", "HEAD^{tree}", "-m", "unrelated base")
	seedCacheObject(t, repo.work, ready.Path, unrelated)
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.2.3", unrelated)
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", derived, true); !errors.Is(err, ErrModulePseudoVersionUnavailable) {
		t.Fatalf("unrelated base: %v", err)
	}
	// A hash-shaped ref may not divert an object-prefix lookup.
	runGit(t, ready.Path, "update-ref", "refs/heads/"+repo.initialSHA[:12], feature)
	if sha, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", v, false); err != nil || sha != repo.initialSHA {
		t.Fatalf("hash-shaped ref changed lookup: %s %v", sha, err)
	}
	// Simulate a force-push removing every public path to a previously allowed
	// source, while retaining the object in private pins.
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.3.0", repo.initialSHA)
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.2.3", repo.initialSHA)
	runGit(t, ready.Path, "update-ref", "-d", "refs/heads/"+repo.initialSHA[:12])
	if _, _, err = resolver.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", fv, true); err == nil {
		t.Fatal("force-pushed source remained available through cache pins")
	}
}

func TestModulePseudoVersionThousandsOfRefs(t *testing.T) {
	repo := newTestRepositoryWithBranch(t, "trunk")
	cache := newTestCache(t, repo.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := strconv.ParseInt(runGit(t, repo.work, "show", "-s", "--format=%ct", repo.initialSHA), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	version := module.PseudoVersion("v0", "", time.Unix(stamp, 0), repo.initialSHA[:12])
	unrelated := runGit(t, repo.work, "commit-tree", "HEAD^{tree}", "-m", "unrelated")
	seedCacheObject(t, repo.work, ready.Path, unrelated)
	tagOnly := runGit(t, repo.work, "commit-tree", "HEAD^{tree}", "-m", "tag only")
	seedCacheObject(t, repo.work, ready.Path, tagOnly)
	seedPublicBranches(t, ready.Path, "many-branches", 6000, repo.initialSHA)
	for _, allow := range []bool{false, true} {
		if sha, _, err := cache.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", version, allow); err != nil || sha != repo.initialSHA {
			t.Fatalf("default ancestry with thousands of refs, opt-in=%v: %s %v", allow, sha, err)
		}
	}
	// Move default to unrelated history, keeping many other public branches.
	runGit(t, ready.Path, "update-ref", "refs/heads/trunk", unrelated)
	if _, _, err = cache.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", version, false); !errors.Is(err, ErrModulePseudoVersionUnavailable) {
		t.Fatalf("default policy widened: %v", err)
	}
	if _, _, err = cache.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", version, true); err != nil {
		t.Fatalf("many opt-in branches: %v", err)
	}
	// Tag-only ancestry must scan past thousands of ineligible tags without
	// accepting the internal pin or truncating the eligible tag at the end.
	stamp, err = strconv.ParseInt(runGit(t, ready.Path, "show", "-s", "--format=%ct", tagOnly), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	tagVersion := module.PseudoVersion("v0", "", time.Unix(stamp, 0), tagOnly[:12])
	seedRefs(t, ready.Path, "refs/tags/ineligible", 6000, tagOnly)
	if _, _, err = cache.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", tagVersion, true); !errors.Is(err, ErrModulePseudoVersionUnavailable) {
		t.Fatalf("ineligible tags: %v", err)
	}
	runGit(t, ready.Path, "update-ref", "refs/tags/v1.2.3", tagOnly)
	if _, _, err = cache.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", tagVersion, true); err != nil {
		t.Fatalf("eligible tag after thousands of refs: %v", err)
	}
}

type pseudoCollisionBackend struct {
	gitBackend
	source, collision, kind string
	includeCommit           bool
}

func (b pseudoCollisionBackend) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	if len(spec.args) == 2 && spec.args[0] == "rev-parse" && strings.HasPrefix(spec.args[1], "--disambiguate=") {
		matches := b.collision + "\n"
		if b.includeCommit {
			matches += b.source + "\n"
		}
		return matches, nil
	}
	if len(spec.args) > 0 && spec.args[0] == "cat-file" && spec.stdin != nil {
		query, err := io.ReadAll(spec.stdin)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(query)) == b.collision {
			_, err := fmt.Fprintf(spec.stdout, "%s %s\n", b.collision, b.kind)
			return "", err
		}
		spec.stdin = strings.NewReader(string(query))
	}
	return b.gitBackend.execute(ctx, spec, capture)
}

func TestModulePseudoVersionCommitPrefixCollisions(t *testing.T) {
	repo := newTestRepositoryWithBranch(t, "trunk")
	cache := newTestCache(t, repo.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	stamp, err := strconv.ParseInt(runGit(t, repo.work, "show", "-s", "--format=%ct", repo.initialSHA), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	version := module.PseudoVersion("v0", "", time.Unix(stamp, 0), repo.initialSHA[:12])
	// Real 48-bit collisions are impractical fixtures. Only disambiguation and
	// the colliding object's type are scripted; all other Git reads stay real.
	collision := repo.initialSHA[:39] + "0"
	if collision == repo.initialSHA {
		collision = repo.initialSHA[:39] + "1"
	}
	backend := cache.backend
	for _, kind := range []string{"blob", "tree", "tag", "commit"} {
		for _, include := range []bool{false, true} {
			if kind == "commit" && !include {
				continue
			}
			t.Run(fmt.Sprintf("%s/commit=%v", kind, include), func(t *testing.T) {
				cache.backend = pseudoCollisionBackend{gitBackend: backend, source: repo.initialSHA, collision: collision, kind: kind, includeCommit: include}
				sha, _, err := cache.ModulePseudoVersionSnapshot(t.Context(), "example", "go.example.test/module", version, false)
				if include && kind != "commit" {
					if err != nil || sha != repo.initialSHA {
						t.Fatalf("noncommit collision refused commit: %s %v", sha, err)
					}
				} else if !errors.Is(err, ErrModulePseudoVersionUnavailable) {
					t.Fatalf("nonunique commit accepted: %s %v", sha, err)
				}
			})
		}
	}
}
