package admissioncontract

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func sourceBindingFixture() sourcecontract.Binding {
	commit := strings.Repeat("a", 40)
	return sourcecontract.Binding{
		Template: "main", Artifact: "source", Path: "/work/src", RepositoryID: 1,
		Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main},
		Revision:   sourcecontract.Revision{Repository: "forge/org/repo", Commit: commit},
		Export: sourcecontract.Export{
			Ref:           sourcecontract.Ref{Name: "refs/heads/run", ObjectSHA: commit, CommitSHA: commit},
			DefaultBranch: sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: commit, CommitSHA: commit},
			Tags:          []sourcecontract.Ref{{Name: "refs/tags/v1", ObjectSHA: strings.Repeat("b", 40), CommitSHA: commit}},
		},
	}
}

func TestSourceBindingFixtureIsAnAdmissibleMainExport(t *testing.T) {
	if err := sourceBindingFixture().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSealedSourceTagsAreIndependentOfProducerAndAccessor(t *testing.T) {
	for _, origin := range []string{"producer", "accessor"} {
		t.Run(origin, func(t *testing.T) {
			input := preparedFixture()
			sealed := mustSeal(t, input)
			before, err := sealed.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			copy := input
			if origin == "accessor" {
				copy = sealed.View()
			}
			copy.SourceBindings[0].Export.Tags[0].ObjectSHA = strings.Repeat("d", 40)
			after, err := sealed.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("source tag mutation changed sealed bytes")
			}
		})
	}
}

func TestBindingCopiesPreserveNilAndEmptyExportTags(t *testing.T) {
	for _, tags := range [][]sourcecontract.Ref{nil, {}} {
		bindings := []sourcecontract.Binding{{Export: sourcecontract.Export{Tags: tags}}}
		hook := TransformInput{SourceBindings: bindings}
		if !reflect.DeepEqual(hook, hook.Clone()) {
			t.Fatal("cloning changed tag presence")
		}
	}
}

func TestSourceExportFactsAreBoundByThePreparedDigest(t *testing.T) {
	original := mustSeal(t, preparedFixture()).View().Digest
	for _, tc := range []struct {
		name   string
		change func(*sourcecontract.Binding)
	}{
		{"repository_id", func(b *sourcecontract.Binding) { b.RepositoryID++ }},
		{"revision", func(b *sourcecontract.Binding) { b.Revision.Commit = strings.Repeat("d", 40) }},
		{"own_ref", func(b *sourcecontract.Binding) { b.Export.Ref.ObjectSHA = strings.Repeat("d", 40) }},
		{"default_head", func(b *sourcecontract.Binding) { b.Export.DefaultBranch.ObjectSHA = strings.Repeat("d", 40) }},
		{"tag_name", func(b *sourcecontract.Binding) { b.Export.Tags[0].Name = "refs/tags/other" }},
		{"tag_object", func(b *sourcecontract.Binding) { b.Export.Tags[0].ObjectSHA = strings.Repeat("d", 40) }},
		{"tag_commit", func(b *sourcecontract.Binding) { b.Export.Tags[0].CommitSHA = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := preparedFixture()
			tc.change(&view.SourceBindings[0])
			if mustSeal(t, view).View().Digest == original {
				t.Fatal("changed export fact is not bound")
			}
		})
	}
}

func TestSealedExportDigestIgnoresTagOrderAndPreservesProducerOrder(t *testing.T) {
	input := preparedFixture()
	input.SourceBindings[0].Export.Tags = append(input.SourceBindings[0].Export.Tags,
		sourcecontract.Ref{Name: "refs/tags/a", ObjectSHA: strings.Repeat("e", 40)})
	if err := input.SourceBindings[0].Validate(); err != nil {
		t.Fatal(err)
	}
	before := slices.Clone(input.SourceBindings[0].Export.Tags)
	first := mustSeal(t, input)
	if !reflect.DeepEqual(input.SourceBindings[0].Export.Tags, before) {
		t.Fatal("Seal changed producer tag order")
	}
	other := input.Clone()
	slices.Reverse(other.SourceBindings[0].Export.Tags)
	second := mustSeal(t, other)
	if first.View().Digest != second.View().Digest {
		t.Fatal("producer tag order changed prepared digest")
	}
	firstJSON, err := first.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("producer tag order changed canonical prepared JSON")
	}
}
