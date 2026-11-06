package protectedtests

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func handoffFixture() (Execution, HandoffExpectation, []Recipe) {
	c, recipe := descriptorFixture()
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	i := Intent{RunID: "run", RepoID: 1, Repository: "forge/org/repo", CandidateSHA: strings.Repeat("d", 40), BaselineSHA: strings.Repeat("e", 40), Contract: c, Recipe: recipe, Namespace: "pipelines", WorkflowName: "protected-run", WorkflowSpecDigest: strings.Repeat("f", 64), InventoryDigest: strings.Repeat("a", 64), CaseCount: 2, AdmittedAt: now, Deadline: now.Add(time.Minute)}
	digest := IntentDigest(i)
	r := Receipt{IntentDigest: digest, WorkflowUID: "workflow", Binding: TerminationBinding{Namespace: i.Namespace, WorkflowName: i.WorkflowName, WorkflowUID: "workflow", PodName: "pod", PodUID: "pod-uid", ContainerID: "containerd://id", HarnessImageRef: recipe.HarnessImage, AdmittedAt: now, Deadline: i.Deadline}, Report: Report{Version: 1, IntentDigest: digest, InventoryDigest: i.InventoryDigest, ArtifactSetDigest: strings.Repeat("b", 64), CaseCount: i.CaseCount}, Termination: TerminationRecord{ImageID: "sha256:" + strings.Repeat("c", 64), ContainerID: "containerd://id", PodUID: "pod-uid", WorkflowUID: "workflow", StartedAt: now, FinishedAt: now}, FinishedAt: now}
	return Execution{Intent: i, WorkflowUID: "workflow", Submitted: true, Cleaned: true, Receipt: &r}, HandoffExpectation{RunID: i.RunID, RepoID: i.RepoID, Repository: i.Repository, CandidateSHA: i.CandidateSHA, BaselineSHA: i.BaselineSHA, Contract: &c}, []Recipe{recipe}
}

func TestHandoffRequiresCurrentAuthorityAndCompleteDurableEvidence(t *testing.T) {
	e, want, recipes := handoffFixture()
	approve := func(r Recipe) error { return RecipeAllowed(r, recipes) }
	if err := e.ValidateHandoff(want, approve); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateHandoff(want, nil); err == nil {
		t.Fatal("missing authority accepted")
	}
	revoked := errors.New("approval revoked")
	if err := e.ValidateHandoff(want, func(Recipe) error { return revoked }); !errors.Is(err, revoked) {
		t.Fatalf("revocation lost: %v", err)
	}
	for name, mutate := range map[string]func(*Execution, *HandoffExpectation){
		"unsubmitted": func(e *Execution, _ *HandoffExpectation) { e.Submitted = false },
		"unclean":     func(e *Execution, _ *HandoffExpectation) { e.Cleaned = false },
		"rejected":    func(e *Execution, _ *HandoffExpectation) { e.Rejected = true },
		"refused": func(e *Execution, _ *HandoffExpectation) {
			e.Refusal = &Refusal{Code: "deadline", ObservedAt: e.Intent.AdmittedAt}
		},
		"no receipt":                 func(e *Execution, _ *HandoffExpectation) { e.Receipt = nil },
		"no workflow UID":            func(e *Execution, _ *HandoffExpectation) { e.WorkflowUID = "" },
		"replaced workflow":          func(e *Execution, _ *HandoffExpectation) { e.WorkflowUID = "replacement" },
		"other run":                  func(_ *Execution, w *HandoffExpectation) { w.RunID = "retry" },
		"other repository ID":        func(_ *Execution, w *HandoffExpectation) { w.RepoID++ },
		"other repository namespace": func(_ *Execution, w *HandoffExpectation) { w.Repository = "forge/other/repo" },
		"other candidate":            func(_ *Execution, w *HandoffExpectation) { w.CandidateSHA = strings.Repeat("1", 40) },
		"other baseline":             func(_ *Execution, w *HandoffExpectation) { w.BaselineSHA = strings.Repeat("1", 40) },
		"missing policy":             func(_ *Execution, w *HandoffExpectation) { w.Contract = nil },
		"changed artifact contract":  func(_ *Execution, w *HandoffExpectation) { w.Contract.Artifacts = []string{"build/substitute"} },
		"forged intent":              func(e *Execution, _ *HandoffExpectation) { e.Receipt.IntentDigest = strings.Repeat("0", 64) },
		"missing inventory cases":    func(e *Execution, _ *HandoffExpectation) { e.Receipt.Report.CaseCount-- },
		"extra inventory cases":      func(e *Execution, _ *HandoffExpectation) { e.Receipt.Report.CaseCount++ },
		"changed inventory":          func(e *Execution, _ *HandoffExpectation) { e.Receipt.Report.InventoryDigest = strings.Repeat("0", 64) },
		"missing artifact proof":     func(e *Execution, _ *HandoffExpectation) { e.Receipt.Report.ArtifactSetDigest = "" },
		"wrong pod":                  func(e *Execution, _ *HandoffExpectation) { e.Receipt.Termination.PodUID = "replacement" },
		"wrong image": func(e *Execution, _ *HandoffExpectation) {
			e.Receipt.Termination.ImageID = "sha256:" + strings.Repeat("0", 64)
		},
		"restart": func(e *Execution, _ *HandoffExpectation) { e.Receipt.Termination.RestartCount = 1 },
		"exit":    func(e *Execution, _ *HandoffExpectation) { e.Receipt.Termination.ExitCode = 1 },
		"signal":  func(e *Execution, _ *HandoffExpectation) { e.Receipt.Termination.Signal = 9 },
		"deadline": func(e *Execution, _ *HandoffExpectation) {
			e.Receipt.Termination.FinishedAt = e.Intent.Deadline.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, w, recipes := handoffFixture()
			mutate(&e, &w)
			if err := e.ValidateHandoff(w, func(r Recipe) error { return RecipeAllowed(r, recipes) }); err == nil {
				t.Fatal("unsafe handoff accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*Recipe){
		"removed":        func(r *Recipe) { *r = Recipe{} },
		"harness":        func(r *Recipe) { r.HarnessImage = "registry.example/new@sha256:" + strings.Repeat("c", 64) },
		"suite endpoint": func(r *Recipe) { r.SuiteURL = "https://example.org/changed" },
		"guest endpoint": func(r *Recipe) { r.GuestURL = "https://example.org/changed" },
	} {
		t.Run(name, func(t *testing.T) {
			e, w, recipes := handoffFixture()
			mutate(&recipes[0])
			if err := e.ValidateHandoff(w, func(r Recipe) error { return RecipeAllowed(r, recipes) }); err == nil {
				t.Fatal("changed authority accepted")
			}
		})
	}
}
