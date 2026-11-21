package nativebinding

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func runBinding() RunBinding {
	return RunBinding{Workflow: Object{"ci", "run-1", "uid-1"}, RepositoryID: 1, RunID: "run-1",
		Revision: sourcecontract.Revision{Repository: "forge/org/repo", Commit: strings.Repeat("a", 40)},
		Ref:      "refs/heads/main", Trigger: "branch", Identity: strings.Repeat("b", 64), SpecSHA256: strings.Repeat("c", 64)}
}

func TestRunBindingRefusesEveryChangedFrozenFact(t *testing.T) {
	want := runBinding()
	if err := want.Verify(want); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RunBinding){
		"namespace":     func(b *RunBinding) { b.Workflow.Namespace += "other" },
		"name":          func(b *RunBinding) { b.Workflow.Name += "other" },
		"uid":           func(b *RunBinding) { b.Workflow.UID += "other" },
		"repository_id": func(b *RunBinding) { b.RepositoryID++ },
		"run_id":        func(b *RunBinding) { b.RunID += "other" },
		"repository":    func(b *RunBinding) { b.Revision.Repository += "other" },
		"commit":        func(b *RunBinding) { b.Revision.Commit = strings.Repeat("d", 40) },
		"ref":           func(b *RunBinding) { b.Ref += "other" },
		"trigger":       func(b *RunBinding) { b.Trigger = "tag" },
		"identity":      func(b *RunBinding) { b.Identity = strings.Repeat("d", 64) },
		"spec":          func(b *RunBinding) { b.SpecSHA256 = strings.Repeat("d", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			got := want
			mutate(&got)
			if want.Verify(got) == nil || got.Verify(want) == nil {
				t.Fatal("changed binding accepted in one direction")
			}
		})
	}
}

func TestRunBindingRefusesIncompleteProofAndPreservesErrorType(t *testing.T) {
	for _, binding := range []RunBinding{{}, {Workflow: Object{"ci", "run", "uid"}}} {
		err := binding.Validate()
		var refusal *Refusal
		if !errors.As(errors.Join(fmt.Errorf("wrapped: %w", err), errors.New("other")), &refusal) {
			t.Fatalf("ownership refusal lost: %v", err)
		}
		if refusal.Reason() == "" || refusal.Remedy() == "" {
			t.Fatal("ownership refusal has no next action")
		}
	}
	binding := runBinding()
	binding.Identity = strings.Repeat("A", 64)
	if binding.Validate() == nil {
		t.Fatal("noncanonical proof digest accepted")
	}
}

func childBinding(t *testing.T) ChildBinding {
	t.Helper()
	parent, err := CronName(1, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	run := runBinding()
	run.Trigger = "schedule"
	run.Workflow.Name = parent + "-123"
	run.RunID = run.Workflow.Name
	return ChildBinding{Run: run, Parent: Object{"ci", parent, "parent-uid"}, ScheduleEntry: "nightly", CreatedAt: time.Unix(123, 0).UTC()}
}

func TestChildBindingRetainsExactParentProof(t *testing.T) {
	want := childBinding(t)
	if err := want.Verify(want); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ChildBinding){
		"missing_parent_uid": func(b *ChildBinding) { b.Parent.UID = "" },
		"missing_entry":      func(b *ChildBinding) { b.ScheduleEntry = "" },
		"run_uid":            func(b *ChildBinding) { b.Run.Workflow.UID += "other" },
		"parent_uid":         func(b *ChildBinding) { b.Parent.UID += "other" },
		"parent_name":        func(b *ChildBinding) { b.Parent.Name += "other" },
		"namespace":          func(b *ChildBinding) { b.Parent.Namespace += "other" },
		"entry":              func(b *ChildBinding) { b.ScheduleEntry += "other" },
		"time":               func(b *ChildBinding) { b.CreatedAt = b.CreatedAt.Add(time.Second) },
		"missing_time":       func(b *ChildBinding) { b.CreatedAt = time.Time{} },
		"trigger":            func(b *ChildBinding) { b.Run.Trigger = "branch" },
		"run_id":             func(b *ChildBinding) { b.Run.RunID = "other" },
		"repository":         func(b *ChildBinding) { b.Run.RepositoryID++ },
		"child_suffix": func(b *ChildBinding) {
			b.Run.Workflow.Name = b.Parent.Name + "-0123"
			b.Run.RunID = b.Run.Workflow.Name
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := want
			mutate(&got)
			if want.Verify(got) == nil {
				t.Fatal("changed child proof accepted")
			}
		})
	}
}

func TestCronNameRoundTripAndBound(t *testing.T) {
	for _, id := range []int64{1, 35, 36, math.MaxInt64} {
		for _, entry := range []string{"daily", "another-entry", "unicode-λ"} {
			name, err := CronName(id, entry)
			if err != nil || len(name) > 52 {
				t.Fatalf("name = %q, %v", name, err)
			}
			got, err := CronRepositoryID(name, entry)
			if err != nil || got != id {
				t.Fatalf("round trip = %d, %v", got, err)
			}
			if _, err := CronRepositoryID(name, entry+"changed"); err == nil {
				t.Fatal("entry hash mismatch accepted")
			}
		}
	}
}

func TestCronNameRefusesMissingOrMalformedIdentity(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, err := CronName(id, "entry"); err == nil {
			t.Fatal("invalid repository ID accepted")
		}
	}
	if _, err := CronName(1, ""); err == nil {
		t.Fatal("empty entry accepted")
	}
	for _, name := range []string{"", "other", "oberth-cron-01-value", "oberth-cron--1-value"} {
		if _, err := CronRepositoryID(name, "entry"); err == nil {
			t.Fatal("malformed parent accepted")
		}
	}
}

func TestRunBindingRefusesMissingOwnerAndProofFields(t *testing.T) {
	for name, mutate := range map[string]func(*RunBinding){
		"namespace":     func(b *RunBinding) { b.Workflow.Namespace = "" },
		"name":          func(b *RunBinding) { b.Workflow.Name = "" },
		"uid":           func(b *RunBinding) { b.Workflow.UID = "" },
		"repository_id": func(b *RunBinding) { b.RepositoryID = 0 },
		"run_id":        func(b *RunBinding) { b.RunID = "" },
		"repository":    func(b *RunBinding) { b.Revision.Repository = "" },
		"commit":        func(b *RunBinding) { b.Revision.Commit = "" },
		"ref":           func(b *RunBinding) { b.Ref = "" },
		"trigger":       func(b *RunBinding) { b.Trigger = "" },
		"identity":      func(b *RunBinding) { b.Identity = "" },
		"spec":          func(b *RunBinding) { b.SpecSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			binding := runBinding()
			mutate(&binding)
			if binding.Validate() == nil {
				t.Fatal("incomplete ownership proof accepted")
			}
		})
	}
}
