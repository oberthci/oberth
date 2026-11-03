package gitcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionTrainOrderedMergeAndConflictSkipping(t *testing.T) {
	const conflictPath = " leading\nfile.txt"
	for _, skip := range []bool{false, true} {
		t.Run(map[bool]string{false: "refuse", true: "skip"}[skip], func(t *testing.T) {
			repo := newTestRepository(t)
			sources := []model.PromotionSource{}
			for i, name := range []string{"first", "conflict", "last"} {
				runGit(t, repo.work, "checkout", "-B", name, repo.initialSHA)
				path := conflictPath
				if i == 2 {
					path = "last.txt"
				}
				if err := os.WriteFile(filepath.Join(repo.work, path), []byte(name+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo.work, "add", path)
				runGit(t, repo.work, "commit", "-m", name)
				sources = append(sources, model.PromotionSource{SHA: runGit(t, repo.work, "rev-parse", "HEAD"), Ref: name})
			}
			cache, err := New(Config{AllowLocalUpstreams: true, Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repo.upstream, nil }})
			if err != nil {
				t.Fatal(err)
			}
			ready, err := cache.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range sources {
				seedCacheObject(t, repo.work, ready.Path, source.SHA)
			}
			workspace := filepath.Join(t.TempDir(), "train")
			plan, err := cache.PreparePromotionTrain(t.Context(), "example", sources, "main", workspace, skip)
			if !skip {
				var conflict *PromotionTrainConflict
				if !errors.As(err, &conflict) || conflict.Index != 2 || conflict.SHA != sources[1].SHA || len(conflict.Paths) != 1 || conflict.Paths[0] != conflictPath {
					t.Fatalf("conflict identity: %+v %v", plan, err)
				}
				if _, err := os.Stat(workspace); !os.IsNotExist(err) {
					t.Fatal("failed train retained workspace")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if plan.FastForward || plan.BaseSHA != repo.initialSHA || plan.TreeSHA == "" {
					t.Fatalf("wrong plan: %+v", plan)
				}
				if len(plan.Sources) != 3 || !plan.Sources[1].Skipped || len(plan.Sources[1].ConflictPaths) != 1 || plan.Sources[1].ConflictPaths[0] != conflictPath || plan.Sources[0].Skipped || plan.Sources[2].Skipped {
					t.Fatalf("wrong source results: %+v", plan.Sources)
				}
				if got := runGit(t, ready.Path, "show", plan.MergedSHA+":"+conflictPath); got != "first" {
					t.Fatalf("skipped source leaked: %q", got)
				}
				if got := runGit(t, ready.Path, "show", plan.MergedSHA+":last.txt"); got != "last" {
					t.Fatalf("later source missing: %q", got)
				}
				parents := strings.Fields(runGit(t, ready.Path, "show", "-s", "--format=%P", plan.MergedSHA))
				if len(parents) != 2 || parents[1] != sources[2].SHA {
					t.Fatalf("last source out of order: %v", parents)
				}
				if got := runGit(t, ready.Path, "show", "-s", "--format=%P", parents[0]); !strings.HasSuffix(got, " "+sources[0].SHA) {
					t.Fatalf("first source out of order: %s", got)
				}
				if got := runGit(t, ready.Path, "rev-parse", plan.MergedSHA+"^{tree}"); got != plan.TreeSHA {
					t.Fatal("tree identity mismatch")
				}
			}
			if got := remoteRef(t, repo.upstream, "refs/heads/main"); got != repo.initialSHA {
				t.Fatal("planning moved target")
			}
		})
	}
}

func TestPromotionTrainContentMergeAndUnbornTarget(t *testing.T) {
	repo := newTestRepository(t)
	baseText := "first\na\nb\nc\nd\ne\nf\ng\nh\nlast\n"
	if err := os.WriteFile(filepath.Join(repo.work, "file.txt"), []byte(baseText), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.work, "add", "file.txt")
	runGit(t, repo.work, "commit", "-m", "multiline base")
	base := runGit(t, repo.work, "rev-parse", "HEAD")
	runGit(t, repo.work, "push", "origin", "HEAD:main")
	var sources []model.PromotionSource
	for i, name := range []string{"first", "last"} {
		runGit(t, repo.work, "checkout", "-B", name, base)
		content := strings.Replace(baseText, name, name+" changed", 1)
		if err := os.WriteFile(filepath.Join(repo.work, "file.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo.work, "add", "file.txt")
		runGit(t, repo.work, "commit", "-m", name)
		sources = append(sources, model.PromotionSource{SHA: runGit(t, repo.work, "rev-parse", "HEAD"), Ref: fmt.Sprintf("feature/%d", i)})
	}
	cache, err := New(Config{AllowLocalUpstreams: true, Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repo.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		seedCacheObject(t, repo.work, ready.Path, source.SHA)
	}
	for _, target := range []string{"main", "new-target"} {
		plan, err := cache.PreparePromotionTrain(t.Context(), "example", sources, target, filepath.Join(t.TempDir(), "train"), false)
		if err != nil {
			t.Fatal(err)
		}
		content := runGit(t, ready.Path, "show", plan.MergedSHA+":file.txt")
		if !strings.Contains(content, "first changed") || !strings.Contains(content, "last changed") || plan.FastForward || plan.TargetUnborn != (target == "new-target") {
			t.Fatalf("content merge: %+v %s", plan, content)
		}
		if target == "new-target" && plan.BaseSHA != "" {
			t.Fatal("unborn target has base")
		}
	}
	if got := remoteRef(t, repo.upstream, "refs/heads/main"); got != base {
		t.Fatal("planning changed main")
	}
}

func TestPromotionTrainAllConflictsAndOperationalErrors(t *testing.T) {
	repo := newTestRepository(t)
	runGit(t, repo.work, "checkout", "-B", "source", repo.initialSHA)
	if err := os.WriteFile(filepath.Join(repo.work, "file.txt"), []byte("source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.work, "add", "file.txt")
	runGit(t, repo.work, "commit", "-m", "source")
	source := runGit(t, repo.work, "rev-parse", "HEAD")
	runGit(t, repo.work, "checkout", "-B", "main", repo.initialSHA)
	if err := os.WriteFile(filepath.Join(repo.work, "file.txt"), []byte("target\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo.work, "add", "file.txt")
	runGit(t, repo.work, "commit", "-m", "target")
	runGit(t, repo.work, "push", "origin", "main")
	cache, err := New(Config{AllowLocalUpstreams: true, Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repo.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	seedCacheObject(t, repo.work, ready.Path, source)
	sources := []model.PromotionSource{{SHA: source, Ref: "source"}}
	workspace := filepath.Join(t.TempDir(), "train")
	plan, err := cache.PreparePromotionTrain(t.Context(), "example", sources, "main", workspace, true)
	if err == nil || !strings.Contains(err.Error(), "all promotion sources conflict") || !plan.Sources[0].Skipped || plan.MergedSHA != "" {
		t.Fatalf("all skipped: %+v %v", plan, err)
	}
	if _, err = os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("failed checkout retained")
	}
	// A target fetch failure must not be interpreted as a skippable conflict or
	// an unborn branch, even when skip_conflicting is enabled.
	if err = os.Rename(repo.upstream, repo.upstream+"-offline"); err != nil {
		t.Fatal(err)
	}
	plan, err = cache.PreparePromotionTrain(t.Context(), "example", sources, "main", filepath.Join(t.TempDir(), "train"), true)
	if err == nil || plan.TargetUnborn || plan.Sources[0].Skipped {
		t.Fatalf("operational failure skipped: %+v %v", plan, err)
	}
}
