package sourcecontract

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func mainBindingFixture() Binding {
	a, b, c, d := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	return Binding{
		Template: "build", Artifact: "source", Path: "/work/src",
		Descriptor: Descriptor{Version: 1, Kind: Main}, RepositoryID: 42,
		Revision: Revision{Repository: "origin/team/widget", Commit: a},
		Export: Export{
			Ref:           Ref{Name: "refs/heads/feature", ObjectSHA: a, CommitSHA: a},
			DefaultBranch: Ref{Name: "refs/heads/main", ObjectSHA: b, CommitSHA: b},
			Tags:          []Ref{{Name: "refs/tags/v1", ObjectSHA: d, CommitSHA: a}, {Name: "refs/tags/blob", ObjectSHA: c}},
		},
		AuthorizationDigest: strings.Repeat("f", 64),
	}
}

func TestBindingGoldenDigests(t *testing.T) {
	revision := Revision{Repository: "origin/team/lib", Commit: strings.Repeat("e", 40)}
	pin := Binding{Template: "test", Artifact: "library", Path: "/work/sources/lib", RepositoryID: 43, Revision: revision,
		Descriptor: Descriptor{Version: 1, Kind: Pinned, Repository: revision.Repository, Commit: revision.Commit}, AuthorizationDigest: strings.Repeat("f", 64)}
	cases := []struct {
		name, digest string
		binding      Binding
	}{
		{"main", "61e7fd64c0f5a2cdff75dc06d580299e797e90fe68320fd53d6eecc5e843fd13", mainBindingFixture()},
		{"pin", "d351fe6bf2565a758e97a527de79c792fb77fa9a42de8066c8b38dff90eea7de", pin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.binding.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, err := CanonicalJSON(tc.binding)
			if err != nil {
				t.Fatal(err)
			}
			if got := Digest(raw); got != tc.digest {
				t.Fatalf("binding digest = %s; want %s; JSON: %s", got, tc.digest, raw)
			}
		})
	}
}

func TestTagOrderDoesNotChangeBindingDigest(t *testing.T) {
	original := mainBindingFixture()
	saved := original.Clone()
	other := original.Clone()
	slices.Reverse(other.Export.Tags)
	first, err := CanonicalJSON(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalJSON(other)
	if err != nil {
		t.Fatal(err)
	}
	if Digest(first) != Digest(second) {
		t.Fatal("tag producer order changed the binding digest")
	}
	if !reflect.DeepEqual(original, saved) {
		t.Fatal("encoding changed the caller's tag order")
	}
}
