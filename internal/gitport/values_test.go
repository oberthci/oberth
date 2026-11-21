package gitport

import (
	"strings"
	"testing"
)

func request() PromotionRequest {
	revision := Revision{Repository: "origin/org/repo", Commit: strings.Repeat("a", 40)}
	return PromotionRequest{Repository: revision.Repository, Source: revision, Target: Ref{Name: "refs/heads/main"}, ActionReceipt: "action"}
}

func TestRequestValidationAndClones(t *testing.T) {
	r := request()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := r
	clone.Source.Commit = "changed"
	if r.Source.Commit == "changed" {
		t.Fatal("source request aliases input")
	}
	cases := []struct {
		name  string
		check func() error
	}{
		{"zero promotion", func() error { return (PromotionRequest{}).Validate() }},
		{"tag target", func() error { v := r; v.Target.Name = "refs/tags/v1"; return v.Validate() }},
		{"no action", func() error { v := r; v.ActionReceipt = ""; return v.Validate() }},
		{"zero result", func() error { return (Promotion{Request: r}).Validate() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertAuthorRefusal(t, tc.check(), "")
		})
	}
}

func TestFileBoundsAndDetachedBytes(t *testing.T) {
	r := request().Source
	file := File{Path: ".oberth/build.yaml", Blob: r.Commit, Mode: 0100644, Bytes: []byte("source")}
	if err := file.Validate(r); err != nil {
		t.Fatal(err)
	}
	clone := file.Clone()
	clone.Bytes[0] = 'X'
	if file.Bytes[0] == 'X' {
		t.Fatal("file clone aliases source bytes")
	}
	file.Mode = 0120000
	assertAuthorRefusal(t, file.Validate(r), "")
	file.Mode = 0100644
	file.Bytes = make([]byte, 1<<20+1)
	assertAuthorRefusal(t, file.Validate(r), "")
}

func TestResultClonesAreDetached(t *testing.T) {
	ref := Ref{Name: "refs/heads/main"}
	publication := PublicationReceipt{Refs: []Ref{ref}}
	copy := publication.Clone()
	copy.Refs[0].Name = "changed"
	promotion := Promotion{Request: request(), Conflicts: []string{"file"}}
	prepared := promotion.Clone()
	prepared.Conflicts[0] = "changed"
	if publication.Refs[0] != ref || promotion.Conflicts[0] != "file" {
		t.Fatal("result clone aliases retained evidence")
	}
}
