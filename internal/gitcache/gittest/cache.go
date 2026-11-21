package gittest

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"path/filepath"
	"testing"
)

const Repository = "origin/team/widget"

func (f *Fixture) Cache(t *testing.T, live func(context.Context, string) (gitcache.OwnerPage, error)) *gitcache.Cache {
	t.Helper()
	c, err := gitcache.New(t.Context(), gitcache.Config{
		Root: filepath.Join(f.Root, "cache"), Scratch: filepath.Join(f.Root, "scratch"),
		Binary: f.Binary, LiveOwners: live,
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Init(t.Context(), Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "fetch", f.Bare, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	return c
}
func (f *Fixture) Store() string {
	return filepath.Join(f.Root, "cache", "origin", "team", "widget.git")
}
func (f *Fixture) Binding() sourcecontract.Binding {
	return sourcecontract.Binding{
		Template:     "build",
		Artifact:     "source",
		Path:         "/work/src",
		RepositoryID: 1,
		Revision:     sourcecontract.Revision{Repository: Repository, Commit: f.Commit},
		Descriptor:   sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main},
		Export: sourcecontract.Export{
			Ref: sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit},
		},
	}
}

// Bindings constructs valid main bindings with no optional public ref snapshot.
func (f *Fixture) Bindings(commits ...string) []sourcecontract.Binding {
	var bindings []sourcecontract.Binding
	for _, commit := range commits {
		b := f.Binding()
		b.Revision.Commit = commit
		b.Export = sourcecontract.Export{}
		bindings = append(bindings, b)
	}
	return bindings
}
