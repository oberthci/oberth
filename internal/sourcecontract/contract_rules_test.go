package sourcecontract

import (
	"fmt"
	"strings"
	"testing"
)

func TestSourceContractInclusiveBounds(t *testing.T) {
	t.Parallel()
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("document/%+d", delta), func(t *testing.T) {
			body := []byte(strings.Repeat("x", (1<<20)+delta))
			receipt := receiptFixture()
			receipt.DocumentDigest = Digest(body)
			if err := receipt.Validate(body); (err == nil) != (delta <= 0) {
				t.Fatalf("document bytes %d: %v", len(body), err)
			}
		})
		t.Run(fmt.Sprintf("file/%+d", delta), func(t *testing.T) {
			path := strings.Repeat("a/", 2047) + strings.Repeat("x", 2+delta)
			if err := ValidateFilePath(path); (err == nil) != (delta <= 0) {
				t.Fatalf("path bytes %d: %v", len(path), err)
			}
		})
		t.Run(fmt.Sprintf("ref/%+d", delta), func(t *testing.T) {
			ref := "refs/heads/" + strings.Repeat("x", 255+delta)
			if err := ValidatePublicRef(ref); (err == nil) != (delta <= 0) {
				t.Fatalf("ref suffix bytes %d: %v", 255+delta, err)
			}
		})
		t.Run(fmt.Sprintf("tags/%+d", delta), func(t *testing.T) {
			export := Export{Tags: make([]Ref, 4096+delta)}
			for i := range export.Tags {
				export.Tags[i] = Ref{Name: fmt.Sprintf("refs/tags/v%d", i), ObjectSHA: strings.Repeat("a", 40)}
			}
			if err := export.Validate(); (err == nil) != (delta <= 0) {
				t.Fatalf("tag count %d: %v", len(export.Tags), err)
			}
		})
	}
}

func TestPinnedBindingKeepsRepositoryAndExcludesEachGitExport(t *testing.T) {
	t.Parallel()
	revision := receiptFixture().Revision
	base := Binding{Template: "build", Artifact: "source", Path: "/work/sources/library", RepositoryID: 1, Revision: revision, Descriptor: Descriptor{Version: 1, Kind: Pinned, Repository: revision.Repository, Commit: revision.Commit}, AuthorizationDigest: Digest([]byte("grant"))}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Binding)
	}{
		{"another-repository", func(b *Binding) { b.Revision.Repository = "origin/team/other" }},
		{"another-commit", func(b *Binding) { b.Revision.Commit = strings.Repeat("b", 40) }},
		{"own-ref", func(b *Binding) { b.Export.Ref = exportFixture().Ref }},
		{"default-branch", func(b *Binding) { b.Export.DefaultBranch = exportFixture().DefaultBranch }},
		{"tags", func(b *Binding) { b.Export.Tags = exportFixture().Tags }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := base.Clone()
			tc.change(&binding)
			if err := binding.Validate(); err == nil {
				t.Fatal("pin identity or Git metadata restriction was lost")
			}
		})
	}
}

func TestMainDescriptorCannotSelectACommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ repository, commit string }{
		{repository: "origin/team/widget"}, {commit: strings.Repeat("a", 40)}, {repository: "origin/team/widget", commit: strings.Repeat("a", 40)},
	} {
		d := Descriptor{Version: 1, Kind: Main, Repository: tc.repository, Commit: tc.commit}
		if err := d.Validate(); err == nil {
			t.Fatalf("main accepted repository %q and commit %q", d.Repository, d.Commit)
		}
	}
}

func TestExportedBranchRequiresItsCommit(t *testing.T) {
	t.Parallel()
	ref := Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40)}
	if err := ref.Validate(); err == nil {
		t.Fatal("branch without a commit was accepted")
	}
	ref.Name = "refs/tags/blob"
	if err := ref.Validate(); err != nil {
		t.Fatal("tag on a non-commit object was refused", err)
	}
}
