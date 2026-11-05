package gitcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetirementSourcesIncludesFreshAndUnpublishedBranchDeclarations(t *testing.T) {
	r := newTestRepository(t)
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	r.commitFile(t, "pipeline", ".oberth/build.yaml", "kind: Workflow\n")
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "checkout", "-b", "unpublished")
	if err := os.WriteFile(filepath.Join(r.work, ".oberth/tests.yaml"), []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", ".oberth/tests.yaml")
	runGit(t, r.work, "commit", "-m", "legacy")
	sha := runGit(t, r.work, "rev-parse", "HEAD")
	// Publish only to Oberth's cache, not the upstream.
	runGit(t, r.work, "push", ready.Path, sha+":refs/heads/private")
	rows, err := c.RetirementSources(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Path == ".oberth/tests.yaml" && strings.Contains(row.Ref, "private") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unpublished declaration omitted: %+v", rows)
	}
}

func TestRetirementSourcesRefusesSymlinkAndStaleUpstream(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../file.txt", filepath.Join(r.work, ".oberth/build.yaml")); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", ".oberth")
	runGit(t, r.work, "commit", "-m", "link")
	runGit(t, r.work, "push", "origin", "main")
	if _, err := c.RetirementSources(t.Context(), "example"); err == nil {
		t.Fatal("symlink proof accepted")
	}
	if err := os.Rename(r.upstream, r.upstream+"-offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RetirementSources(t.Context(), "example"); err == nil {
		t.Fatal("stale proof accepted")
	}
}

func TestRetirementSourcesSharesImmutableBlobsAcrossDistinctHeads(t *testing.T) {
	r := newTestRepository(t)
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "#" + strings.Repeat("x", (1<<20)-2) + "\n"
	r.commitFile(t, "large shared pipeline", ".oberth/build.yaml", body)
	base := r.commitFile(t, "same blob at another path", ".oberth/release.yaml", body)
	tree := runGit(t, r.work, "rev-parse", "HEAD^{tree}")
	want := map[string]string{}
	for i := 0; i < 20; i++ {
		ref := fmt.Sprintf("retained-%02d", i)
		sha := runGitStdin(t, r.work, ref, "commit-tree", tree, "-p", base)
		runGit(t, r.work, "update-ref", "refs/heads/"+ref, sha)
		want[upstreamRefPrefix+"heads/"+ref] = sha
	}
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	// The fresh call must discover all new upstream heads, not just cached main.
	runGit(t, r.work, "push", "--all", "origin")
	rows, err := c.RetirementSources(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]bool{}
	var shared *byte
	for _, row := range rows {
		if string(row.Body) != body {
			t.Fatal("shared source contents changed")
		}
		if shared == nil {
			shared = &row.Body[0]
		} else if shared != &row.Body[0] {
			t.Fatal("equal immutable blobs retain separate allocations")
		}
		if sha, ok := want[row.Ref]; ok {
			if row.SHA != sha {
				t.Fatal("commit identity changed")
			}
			if seen[row.Ref] == nil {
				seen[row.Ref] = map[string]bool{}
			}
			if seen[row.Ref][row.Path] {
				t.Fatal("duplicate metadata record")
			}
			seen[row.Ref][row.Path] = true
		}
	}
	for ref := range want {
		if len(seen[ref]) != 2 || !seen[ref][".oberth/build.yaml"] || !seen[ref][".oberth/release.yaml"] {
			t.Fatalf("missing metadata for %s: %v", ref, seen[ref])
		}
	}
}

func TestRetirementSourcesRetainsUniqueByteAndFileBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files, size int
		want        string
	}{
		{"unique bytes", 17, 1 << 20, "retirement source inventory exceeds 16 MiB"},
		{"per file", 1, (1 << 20) + 1, "blob exceeds the caller's limit"},
		{"tree entries", 257, 0, "retirement source tree exceeds 256 YAML files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRepository(t)
			if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.files; i++ {
				body := ""
				if tc.size > 0 {
					prefix := fmt.Sprintf("# unique %03d\n", i)
					body = prefix + strings.Repeat("x", tc.size-len(prefix))
				}
				if err := os.WriteFile(filepath.Join(r.work, ".oberth", fmt.Sprintf("%03d.yaml", i)), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, r.work, "add", ".oberth")
			runGit(t, r.work, "commit", "-m", tc.name)
			runGit(t, r.work, "push", "origin", "main")
			rows, err := newTestCache(t, r.upstream).RetirementSources(t.Context(), "example")
			if err == nil || !strings.Contains(err.Error(), tc.want) || rows != nil {
				t.Fatalf("bound accepted: rows=%d err=%v", len(rows), err)
			}
		})
	}
}

func TestRetirementSourcesRetainsMetadataAndRefBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files, refs int
		want        string
	}{
		{"metadata", 256, 33, "retirement source inventory exceeds 8192 entries"},
		{"refs", 1, 1025, "more than 1024 refs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRepository(t)
			if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.files; i++ {
				if err := os.WriteFile(filepath.Join(r.work, ".oberth", fmt.Sprintf("%03d.yaml", i)), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, r.work, "add", ".oberth")
			runGit(t, r.work, "commit", "-m", "bounded aliases")
			runGit(t, r.work, "push", "origin", "main")
			sha := runGit(t, r.work, "rev-parse", "HEAD")
			var updates strings.Builder
			for i := 0; i < tc.refs; i++ {
				fmt.Fprintf(&updates, "create refs/heads/alias-%04d %s\n", i, sha)
			}
			c := newTestCache(t, r.upstream)
			ready, err := c.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			// Batch unpublished public aliases in the synthetic cache. Fetching
			// a thousand new upstream heads would benchmark materialization,
			// rather than the unchanged inventory ref bound being checked.
			runGitStdin(t, ready.Path, updates.String(), "update-ref", "--stdin")
			rows, err := c.RetirementSources(t.Context(), "example")
			if err == nil || !strings.Contains(err.Error(), tc.want) || rows != nil {
				t.Fatalf("bound accepted: rows=%d err=%v", len(rows), err)
			}
		})
	}
}

func TestRetirementSourcesCachedBlobStillChecksModePathAndCancellation(t *testing.T) {
	for _, tc := range []struct{ name, mode, path, want string }{
		{"symlink", "120000", "bad.yaml", "must be a regular Git blob"},
		{"unsafe path", "100644", "line\nbad.yaml", "invalid path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRepository(t)
			if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
				t.Fatal(err)
			}
			sha := r.commitFile(t, "good blob", ".oberth/build.yaml", "kind: Workflow\n")
			blob := runGit(t, r.work, "rev-parse", sha+":.oberth/build.yaml")
			sub := runGitStdin(t, r.work, fmt.Sprintf("%s blob %s\t%s\x00", tc.mode, blob, tc.path), "mktree", "-z")
			tree := runGitStdin(t, r.work, "040000 tree "+sub+"\t.oberth\n", "mktree")
			bad := runGitStdin(t, r.work, "bad entry", "commit-tree", tree, "-p", sha)
			runGit(t, r.work, "push", "origin", bad+":refs/heads/z-bad")
			c := newTestCache(t, r.upstream)
			rows, err := c.RetirementSources(t.Context(), "example")
			if err == nil || !strings.Contains(err.Error(), tc.want) || rows != nil {
				t.Fatalf("cached blob bypassed entry proof: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if rows, err = c.RetirementSources(ctx, "example"); err == nil || rows != nil {
				t.Fatal("cancelled inventory accepted")
			}
		})
	}
}
