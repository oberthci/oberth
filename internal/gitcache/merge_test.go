package gitcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConservativePromotionMerge(t *testing.T) {
	cases := []struct {
		name, left, right string
		conflict          bool
	}{
		{"disjoint paths", "target.txt", "source.txt", false},
		{"identical blob", "file.txt", "file.txt", false},
		{"different blob", "file.txt", "file.txt", true},
		{"nonoverlapping lines still refuse", "file.txt", "file.txt", true},
		{"delete versus modify", "DELETE", "file.txt", true},
		{"identical deletion", "DELETE", "DELETE", false},
		{"file versus directory", "new", "new/child", true},
	}
	for _, backend := range []Backend{BackendCLI, BackendGoGit} {
		t.Run(string(backend), func(t *testing.T) {
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					repository := newTestRepository(t)
					// Multiple lines make the conservative-policy difference explicit: Git's
					// content merge could combine first/last edits, but this policy refuses.
					if test.name == "nonoverlapping lines still refuse" {
						repository.initialSHA = repository.commit(t, "baseline", "first\nsecond\nthird\nfourth\nfifth\n")
					}
					change := func(path, contents, message string) string {
						if path == "DELETE" {
							runGit(t, repository.work, "rm", "file.txt")
						} else {
							if err := os.MkdirAll(filepath.Dir(filepath.Join(repository.work, path)), 0o700); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(repository.work, path), []byte(contents), 0o644); err != nil {
								t.Fatal(err)
							}
							runGit(t, repository.work, "add", path)
						}
						runGit(t, repository.work, "commit", "-m", message)
						return runGit(t, repository.work, "rev-parse", "HEAD")
					}
					runGit(t, repository.work, "checkout", "-b", "feature", repository.initialSHA)
					sourceBody := "source\n"
					targetBody := "target\n"
					if test.name == "identical blob" {
						targetBody = sourceBody
					}
					if test.name == "nonoverlapping lines still refuse" {
						sourceBody = "source first\nsecond\nthird\nfourth\nfifth\n"
						targetBody = "first\nsecond\nthird\nfourth\ntarget fifth\n"
					}
					source := change(test.right, sourceBody, "source")
					runGit(t, repository.work, "checkout", "main")
					base := change(test.left, targetBody, "target")
					runGit(t, repository.work, "push", "origin", "main")
					cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: backend, Upstream: func(string) (string, error) { return repository.upstream, nil }})
					if err != nil {
						t.Fatal(err)
					}
					ready, err := cache.Ensure(t.Context(), "example")
					if err != nil {
						t.Fatal(err)
					}
					seedCacheObject(t, repository.work, ready.Path, source)
					workspace := filepath.Join(t.TempDir(), "promotion")
					candidate, err := cache.PreparePromotion(t.Context(), "example", source, "main", workspace)
					if test.conflict {
						if err == nil || !strings.Contains(err.Error(), "rebase onto main") {
							t.Fatalf("expected rebase refusal, got %+v %v", candidate, err)
						}
						if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
							t.Fatalf("refused merge retained workspace: %v", statErr)
						}
						if got := remoteRef(t, repository.upstream, "refs/heads/main"); got != base {
							t.Fatal("refused merge moved upstream")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if candidate.FastForward || candidate.MergedSHA == source || candidate.BaseSHA != base {
						t.Fatalf("wrong merge identity: %+v", candidate)
					}
					if parents := runGit(t, ready.Path, "show", "-s", "--format=%P", candidate.MergedSHA); parents != base+" "+source {
						t.Fatalf("merge parents=%s", parents)
					}
					wantTree := strings.TrimSpace(strings.SplitN(runGit(t, repository.work, "merge-tree", "--write-tree", base, source), "\n", 2)[0])
					if got := runGit(t, ready.Path, "rev-parse", candidate.MergedSHA+"^{tree}"); got != wantTree {
						t.Fatalf("merge tree=%s, git merge-tree=%s", got, wantTree)
					}
					if err := cache.PushPromotion(t.Context(), "example", "main", candidate.MergedSHA); err != nil {
						t.Fatal(err)
					}
					if got := remoteRef(t, repository.upstream, "refs/heads/main"); got != candidate.MergedSHA {
						t.Fatal("merged object not published exactly")
					}
				})
			}
		})
	}
}
