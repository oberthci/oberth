package gitcache_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/contracttest"
	"github.com/oberthci/oberth/internal/gitcache/fake"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestKeepContract(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Git(t, f.Work, "tag", "-a", "release", "-m", "release", f.Commit)
	tag := f.Git(t, f.Work, "rev-parse", "refs/tags/release")
	f.Git(t, f.Work, "push", f.Bare, "refs/tags/release")
	c := f.Cache(t, nil)
	root := sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit}
	f.Write(t, "second", []byte("second"))
	next := f.CommitFile(t, "second")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	binding := f.Binding()
	binding.Export.Ref = sourcecontract.Ref{Name: "refs/tags/release", ObjectSHA: tag, CommitSHA: f.Commit}
	binding.Export.DefaultBranch = sourcecontract.Ref{Name: "refs/heads/default", ObjectSHA: next, CommitSHA: next}
	t.Run("real Git", func(t *testing.T) {
		contracttest.Keep(t, contracttest.Retention{Keep: c.Keep, Root: binding, Has: func(owner, repo, object string) bool {
			return repo == gittest.Repository &&
				f.Git(
					t,
					f.Store(),
					"for-each-ref",
					"--format=%(refname)",
					"refs/oberth/keep/"+owner+"/"+object,
				) != ""
		}})
	})
	memory := fake.New(
		[]sourcecontract.Revision{root, {Repository: gittest.Repository, Commit: next}},
		map[string][]string{gittest.Repository: {tag}},
	)
	t.Run("memory", func(t *testing.T) {
		contracttest.Keep(t, contracttest.Retention{Keep: memory.Keep, Has: memory.Has, Root: binding})
	})
}

// Covers: REQ-C6-002
func TestFreshUpstreamPolicyContract(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	var absent *gitport.AbsentError
	if _, err := readFreshPolicy(t.Context(), c, gittest.Repository, ".oberth/policy.yaml"); !errors.As(err, &absent) {
		t.Fatalf("true absent file: %v", err)
	}
	f.Write(t, ".oberth/policy.yaml", []byte("require: tested\n"))
	next := f.CommitFile(t, "policy")
	f.Git(t, f.Work, "push", f.Bare, "main")
	got, err := readFreshPolicy(t.Context(), c, gittest.Repository, ".oberth/policy.yaml")
	if err != nil || string(got.Bytes) != "require: tested\n" {
		t.Fatalf("fresh read: %+v %v", got, err)
	}
	if next == f.Commit {
		t.Fatal("no policy change")
	}
	f.Git(t, f.Bare, "update-ref", "refs/heads/feature", f.Commit)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	if _, err = readFreshPolicy(t.Context(), c, gittest.Repository, ".oberth/policy.yaml"); err == nil ||
		errors.As(err, &absent) {
		t.Fatalf("deleted default became absence: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = readFreshPolicy(ctx, c, gittest.Repository, ".oberth/policy.yaml"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
func TestPushOptionsAreExact(t *testing.T) {
	t.Parallel()
	for _, options := range [][]string{{"oberth.assist=1", "oberth.run=0"}, {"oberth.run=1"}, {"oberth.run=0"}, nil} {
		if _, err := gitcache.ParsePushOptions(options); err != nil {
			t.Fatal(err)
		}
	}
	for _, options := range [][]string{
		{"oberth.priority=1"},
		{"oberth.run=true"},
		{"oberth.run"},
		{"unknown=1"},
		{"oberth.run=1", "oberth.run=0"},
		{"oberth.assist=1", "oberth.assist=1"},
	} {
		if _, err := gitcache.ParsePushOptions(options); err == nil {
			t.Fatal(options)
		}
	}
	assist, err := gitcache.ParsePushOptions([]string{"oberth.assist=1"})
	if err != nil || assist.Run || !assist.Assist {
		t.Fatal("assistance requested CI", assist, err)
	}
	got, err := gitcache.ParsePushOptions([]string{"oberth.assist=1", "oberth.run=0"})
	if err != nil || !got.Assist || got.Run {
		t.Fatal(got, err)
	}
}
func TestRemoveNamesRetainingRun(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	if err := c.Keep(t.Context(), "active-run", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(t.Context(), gittest.Repository); err == nil || !strings.Contains(err.Error(), "active-run") {
		t.Fatalf("remove: %v", err)
	}
}

func readFreshPolicy(ctx context.Context, port gitport.Git, repo, file string) (gitport.File, error) {
	head, err := port.RemoteRef(ctx, repo, "refs/heads/main")
	if err != nil {
		return gitport.File{}, err
	}
	return port.ReadFile(ctx, gitport.Revision{Repository: repo, Commit: head.CommitSHA}, file)
}

func TestFreshPolicyReadFailureDoesNotReuseLastFile(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, "policy", []byte("protected\n"))
	f.CommitFile(t, "policy")
	f.Git(t, f.Work, "push", f.Bare, "main")
	c := f.Cache(t, nil)
	if _, err := readFreshPolicy(t.Context(), c, gittest.Repository, "policy"); err != nil {
		t.Fatal(err)
	}
	offline := f.Bare + ".offline"
	if err := os.Rename(f.Bare, offline); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(offline, f.Bare); err != nil {
			t.Error(err)
		}
	}()
	_, err := readFreshPolicy(t.Context(), c, gittest.Repository, "policy")
	var absent *gitport.AbsentError
	var unavailable *gitport.UnavailableError
	if !errors.As(err, &unavailable) || errors.As(err, &absent) {
		t.Fatalf("failed read became absence: %v", err)
	}
}
