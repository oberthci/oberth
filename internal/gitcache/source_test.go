package gitcache_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func readArchive(t *testing.T, body io.ReadCloser) map[string][]byte {
	t.Helper()
	defer func() {
		if err := body.Close(); err != nil {
			t.Error(err)
		}
	}()
	gz, err := gzip.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, e := tr.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Typeflag != tar.TypeReg || h.Mode&0222 != 0 {
			t.Fatalf("writable or nonregular source: %+v", h)
		}
		b, e := io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
		files[h.Name] = b
	}
	return files
}

func TestKeepPreservesEveryRootThroughGC(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	var live []gitcache.Owner
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		return gitcache.OwnerPage{Owners: live}, nil
	})
	root := sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit}
	if err := c.Keep(t.Context(), "run-1", f.Bindings(root.Commit, root.Commit)); err != nil {
		t.Fatal(err)
	}
	live = []gitcache.Owner{{ID: "run-1", Bindings: f.Bindings(root.Commit)}}
	f.Git(t, f.Store(), "update-ref", "-d", "refs/heads/main")
	f.Git(t, f.Store(), "update-ref", "-d", "refs/oberth/upstream/heads/main")
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	blob, err := c.ReadFile(t.Context(), root, "file.txt")
	if err != nil || string(blob.Bytes) != "exact source\n" {
		t.Fatalf("retention: %+v %v", blob, err)
	}
	refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/")
	if strings.Count(refs, "refs/oberth/keep/") != 1 {
		t.Fatal(refs)
	}
	live = nil
	if err = c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReadFile(t.Context(), root, "file.txt"); err == nil {
		t.Fatal("unowned source survived GC")
	}
}
func TestRestoreRepinsBeforeDeletingOrphans(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	fail := false
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		if fail {
			return gitcache.OwnerPage{}, io.ErrUnexpectedEOF
		}
		return gitcache.OwnerPage{Owners: []gitcache.Owner{{ID: "live", Bindings: f.Bindings(f.Commit)}}}, nil
	})
	if err := c.Keep(t.Context(), "orphan", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := c.GC(t.Context()); err == nil {
		t.Fatal("failed read authorized GC")
	}
	if !strings.Contains(f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)"), "/orphan/") {
		t.Fatal("failed read removed orphan")
	}
	fail = false
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "/orphan/") || !strings.Contains(refs, "/live/") {
		t.Fatal(refs)
	}
}
// A live owner whose recorded source is gone stops deletion only in its own
// repository: another repository is collected, archived and removable.
func TestGoneOwnerStopsCollectionOnlyInItsRepository(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"cache", "./cache/./"} {
		t.Run(root, func(t *testing.T) {
			t.Parallel()
			f := gittest.New(t)
			gone := f.Bindings(strings.Repeat("a", 40))
			gone[0].Revision.Repository = "origin/team/other"
			c := goneOwnerCache(t, f, f.Root+"/"+root, []gitcache.Owner{{ID: "gone", Bindings: gone}, {ID: "live", Bindings: f.Bindings(f.Commit)}})
			if err := c.Keep(t.Context(), "orphan", f.Bindings(f.Commit)); err != nil {
				t.Fatal(err)
			}
			if err := c.GC(t.Context()); err == nil || !strings.Contains(err.Error(), "other.git") || strings.Contains(err.Error(), "widget.git") {
				t.Fatal("gone owner's repository was collected, or another one was stopped", err)
			}
			refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)")
			if strings.Contains(refs, "/orphan/") || !strings.Contains(refs, "/live/") {
				t.Fatal(refs)
			}
		})
	}
}

// goneOwnerCache builds a cache at root, which may be spelled uncleanly, holding
// the fixture repository and an empty second one.
func goneOwnerCache(t *testing.T, f *gittest.Fixture, root string, owners []gitcache.Owner) *gitcache.Cache {
	t.Helper()
	c, err := gitcache.New(t.Context(), gitcache.Config{
		Root: root, Scratch: filepath.Join(f.Root, "scratch"), Binary: f.Binary,
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{Owners: owners}, nil },
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, repo := range []string{gittest.Repository, "origin/team/other"} {
		if err = c.Init(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
	}
	f.Git(t, f.Store(), "fetch", f.Bare, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	return c
}
func TestKeepVerifiesAllRootsBeforeTransaction(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	roots := f.Bindings(f.Commit, strings.Repeat("a", 40))
	if err := c.Keep(t.Context(), "run-1", roots); err == nil {
		t.Fatal("missing root accepted")
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); got != "" {
		t.Fatal(got)
	}
}
func TestMainArchiveHasExactHistoryAndFrozenInventory(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Git(t, f.Work, "tag", "-a", "v1", "-m", "tag")
	f.Git(t, f.Work, "branch", "unrelated")
	f.Git(t, f.Work, "push", f.Bare, "--tags")
	c := f.Cache(t, nil)
	b := f.Binding()
	tag, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/tags/v1")
	if err != nil {
		t.Fatal(err)
	}
	b.Export.Tags = []sourcecontract.Ref{tag}
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	if string(files["file.txt"]) != "exact source\n" || string(files[".git/HEAD"]) != f.Commit+"\n" {
		t.Fatal("wrong admitted bytes")
	}
	for name := range files {
		if strings.Contains(name, "unrelated") || name == ".git/config" || strings.Contains(name, "hooks") {
			t.Fatalf("exported %s", name)
		}
	}
	checkout := filepath.Join(f.Root, "checkout")
	writeCheckout(t, checkout, files)

	if got := f.Git(t, checkout, "rev-parse", "HEAD"); got != f.Commit {
		t.Fatal(got)
	}
	if got := f.Git(t, checkout, "status", "--porcelain"); got != "" {
		t.Fatalf("unclean checkout: %s", got)
	}
	if got := f.Git(t, checkout, "rev-parse", "refs/tags/v1"); got != tag.ObjectSHA {
		t.Fatal(got)
	}
}
func TestPinArchiveRejectsLinksAndOmitsGit(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	b := f.Binding()
	b.Descriptor = sourcecontract.Descriptor{
		Version:    1,
		Kind:       sourcecontract.Pinned,
		Repository: gittest.Repository,
		Commit:     f.Commit,
	}
	b.Path = "/work/sources/input"
	b.Export = sourcecontract.Export{}
	b.AuthorizationDigest = strings.Repeat("a", 64)
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	if len(files) != 1 || string(files["file.txt"]) != "exact source\n" {
		t.Fatal(files)
	}
	if err = os.Symlink("file.txt", filepath.Join(f.Work, "link")); err != nil {
		t.Fatal(err)
	}
	next := f.CommitFile(t, "link")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	b.Revision.Commit = next
	b.Descriptor.Commit = next
	if _, err = c.Stage(t.Context(), b); err == nil {
		t.Fatal("pinned symlink accepted")
	}
}

// Covers: REQ-C6-011
func TestValidatedGitInputsContract(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	for _, repo := range []string{"../escape", "-c x=y", "origin/team/widget/extra", "/origin/team/widget"} {
		if err := c.Init(t.Context(), repo); err == nil {
			t.Fatalf("accepted repository %q", repo)
		}
	}
	for _, path := range []string{"../outside", ".git/config", "/absolute", "file\x00name", strings.Repeat("x", 4097)} {
		_, err := c.ReadFile(t.Context(), sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit}, path)
		var refused *gitcache.RefusalError
		if !errors.As(err, &refused) {
			t.Fatalf("path reached Git: %q: %v", path, err)
		}
	}
	for _, ref := range []string{"--help", "refs/heads/a^{commit}", "refs/heads/a..b", "refs/oberth/keep/run/a"} {
		if _, err := c.ResolveRef(t.Context(), gittest.Repository, ref); err == nil {
			t.Fatal(ref)
		}
	}
	testInvalidUpstreamURLs(t, f)
	if _, err := os.Stat(filepath.Join(f.Root, "escape.git")); !os.IsNotExist(err) {
		t.Fatal("injection had an effect")
	}
}
func TestReadFileExactBytesAndSizeBoundary(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	for _, size := range []int{1 << 20, (1 << 20) + 1} {
		body := []byte(strings.Repeat(" ", size))
		f.Write(t, "bounded", body)
		commit := f.CommitFile(t, "size")
		f.Git(t, f.Store(), "fetch", f.Work, commit)
		got, err := c.ReadFile(
			t.Context(),
			sourcecontract.Revision{Repository: gittest.Repository, Commit: commit},
			"bounded",
		)
		if size == 1<<20 {
			if err != nil || len(got.Bytes) != size {
				t.Fatalf("N: %d %v", len(got.Bytes), err)
			}
		} else if err == nil {
			t.Fatal("N+1 accepted")
		}
	}
	var absent *gitport.AbsentError
	if _, err := c.ReadFile(
		t.Context(),
		sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit},
		"missing",
	); !errors.As(err, &absent) {
		t.Fatalf("wrong absence: %v", err)
	}
}

func writeCheckout(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSHA256SourceHasUsableDetachedGitMetadata(t *testing.T) {
	t.Parallel()
	f := gittest.NewFormat(t, "sha256")
	c := f.Cache(t, nil)
	if len(f.Commit) != 64 {
		t.Fatal("wrong object format")
	}
	stage, err := c.Stage(t.Context(), f.Binding())
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	checkout := filepath.Join(f.Root, "checkout")
	writeCheckout(t, checkout, files)
	if got := f.Git(t, checkout, "rev-parse", "HEAD"); got != f.Commit {
		t.Fatal(got)
	}
	if got := f.Git(t, checkout, "status", "--porcelain"); got != "" {
		t.Fatal(got)
	}
}
func TestActiveSnapshotOutlivesReleaseAndGC(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	if err := c.Keep(t.Context(), "run", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	stage, err := c.Stage(t.Context(), f.Binding())
	if err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "update-ref", "-d", "refs/heads/main")
	f.Git(t, f.Store(), "update-ref", "-d", "refs/oberth/upstream/heads/main")
	if err = c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := readArchive(t, stage.Body); string(got["file.txt"]) != "exact source\n" {
		t.Fatal("snapshot changed after GC")
	}
}
func TestArchiveAndRemovalRefuseActiveOwners(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	c := f.Cache(t, nil)
	if body, err := c.Archive(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	} else {
		if err = body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Keep(t.Context(), "run", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	if body, err := c.Archive(t.Context(), gittest.Repository); err == nil {
		_ = body.Close()
		t.Fatal("archived active source")
	}
}
func TestUploadHidesPrivateRefsAndUsesIndependentSnapshot(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	if err := c.Keep(t.Context(), "private", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	began, ended := false, false
	err := c.Upload(
		t.Context(),
		gittest.Repository,
		gitcache.ServiceIO{
			Input:  io.NopCloser(strings.NewReader("0000")),
			Output: &output,
			Begin:  func() { began = true },
			End:    func() { ended = true },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !began ||
		!ended ||
		!strings.Contains(output.String(), "refs/heads/main") ||
		strings.Contains(output.String(), "refs/oberth/") {
		t.Fatal(output.String())
	}
}

func testInvalidUpstreamURLs(t *testing.T, f *gittest.Fixture) {
	t.Helper()
	endpoint := ""
	c, err := gitcache.New(t.Context(), gitcache.Config{
		Root: filepath.Join(f.Root, "url-cache"), Scratch: filepath.Join(f.Root, "url-scratch"),
		Binary: "/uncontacted/git",
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{
				URL:           endpoint,
				DefaultBranch: "refs/heads/main",
				SSHBinary:     "/uncontacted/ssh",
				SSHConfig:     "/uncontacted/config",
			}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"--upload-pack=anything",
		"ext::anything",
		"https://git@git.example/team/repo.git",
		"git://git@git.example/team/repo.git",
		"ssh://git:password@git.example/team/repo.git",
		"ssh://git@git.example/team/repo.git?command=other",
		"ssh://git@git.example/../outside.git",
		"ssh://git@git.example/%2e%2e/outside.git",
	} {
		endpoint = bad
		_, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main")
		var refused *gitcache.RefusalError
		if !errors.As(err, &refused) {
			t.Fatalf("URL reached a child command: %q: %v", bad, err)
		}
	}
}
