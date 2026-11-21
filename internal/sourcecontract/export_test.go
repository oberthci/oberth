package sourcecontract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func exportFixture() Export {
	id := strings.Repeat("a", 40)
	return Export{Ref: Ref{Name: "refs/heads/feature", ObjectSHA: id, CommitSHA: id}, DefaultBranch: Ref{Name: "refs/heads/main", ObjectSHA: id, CommitSHA: id}, Tags: []Ref{{Name: "refs/tags/tree", ObjectSHA: strings.Repeat("b", 40)}}}
}
func TestExportKeepsRawTagsAndClonesThem(t *testing.T) {
	original := exportFixture()
	if err := original.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := original.Clone()
	clone.Tags[0].ObjectSHA = "changed"
	if original.Tags[0].ObjectSHA == "changed" {
		t.Fatal("export clone shared tag storage")
	}
	b := Binding{Export: original}
	copy := b.Clone()
	copy.Export.Tags[0].Name = "changed"
	if b.Export.Tags[0].Name == "changed" {
		t.Fatal("binding clone shared tag storage")
	}
	raw, err := CanonicalJSON(original)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Export
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	again, err := CanonicalJSON(roundTrip)
	if err != nil || string(again) != string(raw) {
		t.Fatal("export round trip changed the snapshot")
	}
}
func TestExportRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Export)
	}{
		{"bad own ref", func(e *Export) { e.Ref.Name = "refs/heads/x..y" }},
		{"bad default ref", func(e *Export) { e.DefaultBranch.ObjectSHA = "short" }},
		{"tag as default", func(e *Export) { e.DefaultBranch = e.Tags[0] }},
		{"other branch", func(e *Export) { e.Tags[0] = e.DefaultBranch }},
		{"tag object", func(e *Export) { e.Tags[0].ObjectSHA = "short" }},
		{"tag commit", func(e *Export) { e.Tags[0].CommitSHA = "short" }},
		{"duplicate tag", func(e *Export) { e.Tags = append(e.Tags, e.Tags[0]) }},
		{"tag bound", func(e *Export) {
			e.Tags = make([]Ref, MaxRefs+1)
			for i := range e.Tags {
				e.Tags[i] = Ref{Name: fmt.Sprintf("refs/tags/t%04d", i), ObjectSHA: strings.Repeat("a", 40)}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := exportFixture()
			tc.change(&e)
			if e.Validate() == nil {
				t.Fatal("invalid export accepted")
			}
		})
	}
}
func TestExportGoldenStructEncoding(t *testing.T) {
	input := Export{Tags: []Ref{}}
	raw, err := CanonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"default_branch":{"commit_sha":"","object_sha":"","ref":""},"ref":{"commit_sha":"","object_sha":"","ref":""},"tags":[]}`
	if string(raw) != want {
		t.Fatalf("export wire = %s; want %s", raw, want)
	}
}
