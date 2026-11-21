package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

type accountCarrier struct {
	name  string
	setup func(*wfv1.Workflow, *Policy)
	value func(*wfv1.Workflow) *string
}

func TestIdentityReplacesAuthoredAccountsInEveryCarrier(t *testing.T) {
	t.Parallel()
	carriers := append(workflowAccountCarriers(), templateAccountCarriers()...)
	carriers = append(carriers, nestedAccountCarriers()...)
	for _, tc := range carriers {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []admissioncontract.Mode{admissioncontract.ModeInspect, admissioncontract.ModeSubmit} {
				for _, authored := range []string{"", "author-picked", "default", "{{workflow.parameters.account}}"} {
					assertAccountReplacement(t, tc, mode, authored)
				}
			}
		})
	}
}

func assertAccountReplacement(t *testing.T, tc accountCarrier, mode admissioncontract.Mode, authored string) {
	t.Helper()
	w, policy := simpleWorkflow(), Policy{}
	if tc.setup != nil {
		tc.setup(w, &policy)
	}
	*tc.value(w) = authored
	r := requestFor(t, w)
	r.Mode, r.IdentityRequest.Mode = mode, mode
	sealed, err := Prepare(t.Context(), r, policy, identityHooks())
	if err != nil {
		t.Fatalf("mode %s, authored %q: %v", mode, authored, err)
	}
	if got := *tc.value(sealed.View().Workflow); got != r.IdentityRequest.PipelineAccount {
		t.Fatalf("mode %s: authored %q became %q, want %q", mode, authored, got, r.IdentityRequest.PipelineAccount)
	}
}

func workflowAccountCarriers() []accountCarrier {
	return []accountCarrier{
		{"workflow", nil, func(w *wfv1.Workflow) *string { return &w.Spec.ServiceAccountName }},
		{"workflow-executor", func(w *wfv1.Workflow, _ *Policy) { w.Spec.Executor = &wfv1.ExecutorConfig{} },
			func(w *wfv1.Workflow) *string { return &w.Spec.Executor.ServiceAccountName }},
		{"defaults", func(w *wfv1.Workflow, _ *Policy) { w.Spec.TemplateDefaults = &wfv1.Template{} },
			func(w *wfv1.Workflow) *string { return &w.Spec.TemplateDefaults.ServiceAccountName }},
		{"defaults-executor", func(w *wfv1.Workflow, _ *Policy) {
			w.Spec.TemplateDefaults = &wfv1.Template{Executor: &wfv1.ExecutorConfig{}}
		},
			func(w *wfv1.Workflow) *string { return &w.Spec.TemplateDefaults.Executor.ServiceAccountName }},
		{"artifact-gc", func(w *wfv1.Workflow, _ *Policy) { w.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{} },
			func(w *wfv1.Workflow) *string { return &w.Spec.ArtifactGC.ServiceAccountName }},
	}
}

func templateAccountCarriers() []accountCarrier {
	return []accountCarrier{
		{"template", nil, func(w *wfv1.Workflow) *string { return &w.Spec.Templates[0].ServiceAccountName }},
		{"template-executor", func(w *wfv1.Workflow, _ *Policy) { w.Spec.Templates[0].Executor = &wfv1.ExecutorConfig{} },
			func(w *wfv1.Workflow) *string { return &w.Spec.Templates[0].Executor.ServiceAccountName }},
		{"output-artifact-gc", func(w *wfv1.Workflow, _ *Policy) {
			w.Spec.Templates[0].Outputs.Artifacts = []wfv1.Artifact{{Name: "result", Path: "/out", ArtifactGC: &wfv1.ArtifactGC{}}}
		}, func(w *wfv1.Workflow) *string {
			return &w.Spec.Templates[0].Outputs.Artifacts[0].ArtifactGC.ServiceAccountName
		}},
		{"unused-template", func(w *wfv1.Workflow, _ *Policy) {
			leaf := w.Spec.Templates[0].DeepCopy()
			leaf.Name = "unused"
			w.Spec.Templates = append(w.Spec.Templates, *leaf)
		}, func(w *wfv1.Workflow) *string { return &w.Spec.Templates[1].ServiceAccountName }},
		{"template-reference", func(w *wfv1.Workflow, p *Policy) {
			*w = *sharedConsumer("shared", "work")
			p.TemplateGetter = getterFor(map[string]*wfv1.WorkflowTemplate{"shared": sharedObject("shared")})
		}, func(w *wfv1.Workflow) *string { return &w.Spec.Templates[0].ServiceAccountName }},
	}
}

func nestedAccountCarriers() []accountCarrier {
	return []accountCarrier{
		{"inline-steps", func(w *wfv1.Workflow, _ *Policy) {
			leaf := w.Spec.Templates[0].DeepCopy()
			w.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Inline: leaf}}}}}
		}, func(w *wfv1.Workflow) *string {
			return &w.Spec.Templates[0].Steps[0].Steps[0].Inline.ServiceAccountName
		}},
		{"inline-dag", func(w *wfv1.Workflow, _ *Policy) {
			leaf := w.Spec.Templates[0].DeepCopy()
			w.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: leaf}}}}
		}, func(w *wfv1.Workflow) *string { return &w.Spec.Templates[0].DAG.Tasks[0].Inline.ServiceAccountName }},
		{"exit-handler", func(w *wfv1.Workflow, _ *Policy) {
			leaf := w.Spec.Templates[0].DeepCopy()
			leaf.Name = "finish"
			w.Spec.Templates = append(w.Spec.Templates, *leaf)
			w.Spec.OnExit = "finish"
		}, func(w *wfv1.Workflow) *string { return &w.Spec.Templates[1].ServiceAccountName }},
		{"lifecycle-hook", func(w *wfv1.Workflow, _ *Policy) {
			leaf := w.Spec.Templates[0].DeepCopy()
			leaf.Name = "finish"
			w.Spec.Templates = append(w.Spec.Templates, *leaf)
			w.Spec.Hooks = wfv1.LifecycleHooks{"exit": {Template: "finish"}}
		}, func(w *wfv1.Workflow) *string { return &w.Spec.Templates[1].ServiceAccountName }},
	}
}

func TestIdentityNameRules(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"pipeline", "resource", "namespace"} {
		for _, name := range []string{"assigned-account", "default", "Upper", "account/name"} {
			t.Run(role+"/"+name, func(t *testing.T) {
				r, e := identityForName(role, name)
				err := validateIdentity(r, e, true)
				if name == "assigned-account" || role == "namespace" && name == "default" {
					if err != nil {
						t.Fatal("valid assigned name refused", err)
					}
				} else {
					requireCode(t, err, "identity")
				}
			})
		}
	}
}

func identityForName(role, name string) (admissioncontract.IdentityRequest, admissioncontract.IdentityEvidence) {
	r := admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: "pipeline"}
	e := admissioncontract.IdentityEvidence{Namespace: "ci", Pipeline: admissioncontract.AccountEvidence{Name: "pipeline", UID: "pipeline-uid"}, State: admissioncontract.StateVerified}
	switch role {
	case "pipeline":
		r.PipelineAccount, e.Pipeline.Name = name, name
	case "namespace":
		r.Namespace, e.Namespace = name, name
	case "resource":
		r.ResourceAccount = name
		e.Resource = &admissioncontract.AccountEvidence{Name: name, UID: "resource-uid"}
	}
	return r, e
}

func TestIdentityAccountPatchesRemainRefused(t *testing.T) {
	t.Parallel()
	for _, carrier := range []string{"workflow", "template", "artifact-gc"} {
		t.Run(carrier, func(t *testing.T) {
			w := simpleWorkflow()
			const patch = `{"serviceAccountName":"author-picked"}`
			switch carrier {
			case "workflow":
				w.Spec.PodSpecPatch = patch
			case "template":
				w.Spec.Templates[0].PodSpecPatch = patch
			case "artifact-gc":
				w.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{PodSpecPatch: patch}
			}
			_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
			requireCode(t, err, "patch")
		})
	}
}
