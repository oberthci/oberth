package sourcecontract

import (
	"errors"
	"strings"
	"testing"
)

func receiptFixture() Receipt {
	id := strings.Repeat("a", 40)
	return Receipt{Version: 1, Revision: Revision{Repository: "origin/team/widget", Commit: id}, ObjectSHA: id, File: ".oberth/build.yaml", Blob: id, DocumentDigest: Digest([]byte("document"))}
}
func TestReceiptRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Receipt)
	}{
		{"version", func(r *Receipt) { r.Version = 0 }},
		{"repository", func(r *Receipt) { r.Revision.Repository = "../bad" }},
		{"commit", func(r *Receipt) { r.Revision.Commit = "short" }},
		{"file", func(r *Receipt) { r.File = "../bad" }},
		{"object", func(r *Receipt) { r.ObjectSHA = "short" }},
		{"blob", func(r *Receipt) { r.Blob = "short" }},
		{"digest", func(r *Receipt) { r.DocumentDigest = "short" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := receiptFixture()
			tc.change(&r)
			var target *InvalidError
			if !errors.As(r.Validate([]byte("document")), &target) {
				t.Fatal("missing typed refusal")
			}
			if !strings.HasSuffix(target.Reason(), ".") {
				t.Fatal("reason lacks punctuation")
			}
		})
	}
	for _, body := range [][]byte{nil, make([]byte, MaxFileBytes+1)} {
		r := receiptFixture()
		r.DocumentDigest = Digest(body)
		if r.Validate(body) == nil {
			t.Fatal("empty/oversized document accepted")
		}
	}
}
func TestBindingPinsRequireMatchingRevisionAndNoGitExport(t *testing.T) {
	r := receiptFixture().Revision
	b := Binding{Template: "step", Artifact: "source", Path: "/work/sources/lib", Descriptor: Descriptor{Version: 1, Kind: Pinned, Repository: r.Repository, Commit: r.Commit}, RepositoryID: 1, Revision: r, AuthorizationDigest: Digest([]byte("grant"))}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	mismatch := b
	mismatch.Revision.Commit = strings.Repeat("b", 40)
	if mismatch.Validate() == nil {
		t.Fatal("pin revision mismatch accepted")
	}
	b.Export = exportFixture()
	if b.Validate() == nil {
		t.Fatal("pin exported Git metadata")
	}
}
func TestMainExportNamesTestedCommit(t *testing.T) {
	r := receiptFixture().Revision
	b := Binding{Template: "step", Artifact: "source", Path: "/work/src", Descriptor: Descriptor{Version: 1, Kind: Main}, RepositoryID: 1, Revision: r, Export: exportFixture()}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	b.Export.Ref.CommitSHA = strings.Repeat("b", 40)
	if b.Validate() == nil {
		t.Fatal("own ref did not identify the tested commit")
	}
}
func TestFileAndRefSyntaxRefusals(t *testing.T) {
	for _, name := range []string{"", "/file", "a/../b", "a//b", ".", "a\x00b", "a\rb", "a\nb", "a\\b"} {
		if ValidateFilePath(name) == nil {
			t.Fatalf("unsafe path %q accepted", name)
		}
	}
	for _, ref := range []string{"refs/heads/x.lock", "refs/heads/.hidden", "refs/heads/a//b", "refs/heads/x.", "refs/heads/" + strings.Repeat("x", 256), "refs/heads/x\t", "refs/heads/x@{y", "refs/heads/x\x7f"} {
		if ValidatePublicRef(ref) == nil {
			t.Fatalf("invalid ref %q accepted", ref)
		}
	}
}
