package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOlderRunCannotPublishOverNewerBranchHead(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-fast-forward", true: "feature-force"}[force], func(t *testing.T) {
			repository := newTestRepository(t)
			cache := newTestCache(t, repository.upstream)
			ready, err := cache.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			const branch = "feature/out-of-order"
			older := repository.featureCommit(t, branch, "older\n")
			if err := os.WriteFile(filepath.Join(repository.work, "file.txt"), []byte("newer\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runGit(t, repository.work, "add", "file.txt")
			runGit(t, repository.work, "commit", "-m", "newer")
			newer := runGit(t, repository.work, "rev-parse", "HEAD")
			runGit(t, repository.work, "push", "--force", ready.Path, newer+":refs/heads/"+branch)
			// Two runs were accepted. The newer run finishes first; the older run
			// must not roll its result back when it finishes afterwards.
			if err := cache.SyncBranch(t.Context(), "example", branch, newer, force); err != nil {
				t.Fatal(err)
			}
			if err := cache.SyncBranch(t.Context(), "example", branch, older, force); !errors.Is(err, ErrMirrorBranchAdvanced) {
				t.Fatalf("older run publish=%v; want changed mirror head refusal", err)
			}
			if got := remoteRef(t, repository.upstream, "refs/heads/"+branch); got != newer {
				t.Fatalf("older run rewound upstream to %s; want %s", got, newer)
			}
			if matches, err := cache.MirrorBranchMatches(t.Context(), "example", branch, older); err != nil || matches {
				t.Fatal(matches, err)
			}
		})
	}
}
