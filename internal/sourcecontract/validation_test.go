package sourcecontract

import (
	"errors"
	"strings"
	"testing"
)

func TestDescriptorAndPathRefusals(t *testing.T) {
	cases := []struct {
		name string
		call func() error
	}{
		{"zero descriptor", func() error { return (Descriptor{}).Validate() }},
		{"main selectors", func() error { return (Descriptor{Version: 1, Kind: "main", Repository: "origin/org/repo"}).Validate() }},
		{"missing pin", func() error { return (Descriptor{Version: 1, Kind: "pin"}).Validate() }},
		{"private metadata", func() error { return ValidateFilePath("x/.GIT/config") }},
		{"traversal", func() error { return ValidateFilePath("../file") }},
		{"oversized path", func() error { return ValidateFilePath(strings.Repeat("x", 4097)) }},
		{"private ref", func() error { return ValidatePublicRef("refs/private/x") }},
		{"revision syntax", func() error { return ValidatePublicRef("refs/heads/x~1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var target *InvalidError
			if !errors.As(tc.call(), &target) || target.Remedy() == "" {
				t.Fatal("missing actionable refusal")
			}
		})
	}
}

func TestDescriptorAndPathValidForms(t *testing.T) {
	cases := []error{
		(Descriptor{Version: 1, Kind: "main"}).Validate(),
		(Descriptor{Version: 1, Kind: "pin", Repository: "origin/org/repo", Commit: strings.Repeat("a", 40)}).Validate(),
		ValidateFilePath(".oberth/build.yaml"), ValidatePublicRef("refs/heads/feature/x"),
	}
	for _, err := range cases {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSourceKindValueSet(t *testing.T) {
	for _, kind := range []Kind{Main, Pinned} {
		if !kind.Valid() {
			t.Fatalf("valid kind %q refused", kind)
		}
	}
	for _, kind := range []Kind{"", "unknown", "MAIN"} {
		if kind.Valid() {
			t.Fatalf("invalid kind %q accepted", kind)
		}
	}
}

func TestBindingCannotGrantFromIncompleteDeclarations(t *testing.T) {
	base := Binding{Template: "build", Artifact: "source", Path: "/work/src", Descriptor: Descriptor{Version: 1, Kind: Main}, RepositoryID: 1, Revision: Revision{Repository: "origin/org/repo", Commit: strings.Repeat("a", 40)}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Binding)
	}{
		{"template", func(b *Binding) { b.Template = "" }},
		{"artifact", func(b *Binding) { b.Artifact = "" }},
		{"repository identity", func(b *Binding) { b.RepositoryID = 0 }},
		{"revision", func(b *Binding) { b.Revision.Commit = "" }},
		{"main path", func(b *Binding) { b.Path = "/other" }},
		{"bad authorization", func(b *Binding) { b.AuthorizationDigest = "short" }},
		{"missing pin authorization", func(b *Binding) {
			b.Descriptor = Descriptor{Version: 1, Kind: Pinned, Repository: "origin/org/repo", Commit: strings.Repeat("a", 40)}
			b.Path = "/work/sources/pin"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.change(&changed)
			if changed.Validate() == nil {
				t.Fatal("incomplete declaration accepted")
			}
		})
	}
	pin := base
	pin.Descriptor = Descriptor{Version: 1, Kind: Pinned, Repository: "origin/org/repo", Commit: strings.Repeat("a", 40)}
	pin.Path = "/work/sources/pin"
	pin.AuthorizationDigest = Digest([]byte("approved"))
	if err := pin.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/work/sources/../escape", "/work/sources/x/y", "/work/src", "/work/sources/"} {
		pin.Path = path
		if pin.Validate() == nil {
			t.Fatalf("invalid destination %q accepted", path)
		}
	}
}
