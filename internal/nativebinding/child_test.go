package nativebinding

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChildValidateRefusesEachChildRule(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*ChildBinding)
	}{
		{"parent_uid", "UID is missing", func(b *ChildBinding) { b.Parent.UID = "" }},
		{"namespace", "namespace differs", func(b *ChildBinding) { b.Parent.Namespace = "elsewhere" }},
		{"time", "creation time is missing", func(b *ChildBinding) { b.CreatedAt = time.Time{} }},
		{"trigger", "schedule run trigger", func(b *ChildBinding) { b.Run.Trigger = "branch" }},
		{"run_id", "run ID must equal", func(b *ChildBinding) { b.Run.RunID = "different" }},
		{"parent_name", "parent name does not match", func(b *ChildBinding) { b.Parent.Name = "invalid" }},
		{"entry", "parent name does not match", func(b *ChildBinding) { b.ScheduleEntry = "" }},
		{"repository", "repository differs", func(b *ChildBinding) { b.Run.RepositoryID++ }},
		{"suffix_zero", "positive decimal suffix", func(b *ChildBinding) { setChildName(b, b.Parent.Name+"-0") }},
		{"suffix_padding", "positive decimal suffix", func(b *ChildBinding) { setChildName(b, b.Parent.Name+"-0123") }},
		{"suffix_text", "positive decimal suffix", func(b *ChildBinding) { setChildName(b, b.Parent.Name+"-abc") }},
		{"child_prefix", "positive decimal suffix", func(b *ChildBinding) { setChildName(b, "other-123") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := childBinding(t)
			tc.change(&binding)
			var refusal *Refusal
			if err := binding.Validate(); !errors.As(err, &refusal) || !strings.Contains(refusal.Reason(), tc.reason) {
				t.Fatalf("Validate = %v; want %s", err, tc.reason)
			}
			if binding.Verify(binding) == nil {
				t.Fatal("self-consistent invalid proof accepted")
			}
		})
	}
}

func setChildName(b *ChildBinding, name string) { b.Run.Workflow.Name = name; b.Run.RunID = name }

func TestChildVerifyUsesTimeEquality(t *testing.T) {
	binding := childBinding(t)
	observed := binding
	observed.CreatedAt = binding.CreatedAt.In(time.FixedZone("fixture", 3600))
	if binding.CreatedAt.Location() == observed.CreatedAt.Location() {
		t.Fatal("fixture requires different time representations")
	}
	if err := binding.Verify(observed); err != nil {
		t.Fatalf("same creation instant refused: %v", err)
	}
}

func TestRefusalUsesOneRemedy(t *testing.T) {
	err := &Refusal{reason: "binding changed"}
	if got, want := err.Error(), "native ownership refused: "+err.Reason()+". "+err.Remedy(); got != want {
		t.Fatalf("Error = %q, want %q", got, want)
	}
}
