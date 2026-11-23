package argoworkflow

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestMutexRepositoryAndNamespace(t *testing.T) {
	t.Parallel()
	for _, carrier := range mutexCarriers() {
		t.Run(carrier.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, code, field string
				mutex             *wfv1.Mutex
			}{
				{"own", "", "", &wfv1.Mutex{Name: "forge/org/repo/publish"}},
				{"own namespace", "", "", &wfv1.Mutex{Name: "forge/org/repo/publish", Namespace: "ci"}},
				{"own database", "", "", &wfv1.Mutex{Name: "forge/org/repo/publish", Database: true}},
				{"other repository", "mutex_scope", ".name", &wfv1.Mutex{Name: "forge/org/other/publish"}},
				{"other upstream", "mutex_scope", ".name", &wfv1.Mutex{Name: "other/org/repo/publish"}},
				{"other organization", "mutex_scope", ".name", &wfv1.Mutex{Name: "forge/other/repo/publish"}},
				{"repository prefix", "mutex_scope", ".name", &wfv1.Mutex{Name: "forge/org/repository/publish"}},
				{"unscoped", "mutex_scope", ".name", &wfv1.Mutex{Name: "publish"}},
				{"empty suffix", "mutex_scope", ".name", &wfv1.Mutex{Name: "forge/org/repo/"}},
				{"parameter prefix", "mutex_scope", ".name", &wfv1.Mutex{Name: "{{workflow.parameters.repo}}/publish"}},
				{"parameter suffix", "mutex_scope", ".name", &wfv1.Mutex{Name: "forge/org/repo/{{workflow.parameters.lock}}"}},
				{"other namespace", "mutex_namespace", ".namespace", &wfv1.Mutex{Name: "forge/org/repo/publish", Namespace: "other"}},
				{"parameter namespace", "mutex_namespace", ".namespace", &wfv1.Mutex{Name: "forge/org/repo/publish", Namespace: "{{workflow.namespace}}"}},
				{"other database repository", "mutex_scope", ".name", &wfv1.Mutex{Name: "forge/org/other/publish", Database: true}},
				{"other database namespace", "mutex_namespace", ".namespace", &wfv1.Mutex{Name: "forge/org/repo/publish", Namespace: "other", Database: true}},
				{"null", "sync_null", "", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: identityHooks()}
					// A safe first lock must not conceal a later offender.
					s := &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: "forge/org/repo/first"}, tc.mutex}}
					carrier.set(t, &d, s)
					assertMutexAdmission(t, &d, tc.code, carrier.path+".synchronization.mutexes[1]"+tc.field)
				})
			}
		})
	}
}

func assertMutexAdmission(t *testing.T, d *checkDocument, code, path string) {
	t.Helper()
	d.sync(t)
	before := bytes.Clone(d.request.Document)
	prepared, err := Prepare(t.Context(), d.request, d.policy, d.hooks)
	if !bytes.Equal(before, d.request.Document) {
		t.Fatal("authored document changed")
	}
	if code == "" {
		if err != nil || prepared == nil {
			t.Fatal("repository mutex refused", err)
		}
		return
	}
	if prepared != nil {
		t.Fatal("unsafe mutex admitted")
	}
	for _, f := range admissionFindings(t, err) {
		if f.Code == code && f.Path == path {
			return
		}
	}
	t.Fatalf("missing %s at %s: %v", code, path, err)
}

type mutexCarrier struct {
	name, path string
	set        func(*testing.T, *checkDocument, *wfv1.Synchronization)
}

func mutexCarriers() []mutexCarrier {
	return []mutexCarrier{
		{"workflow", "spec", func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) { d.wf.Spec.Synchronization = s }},
		{"template", `spec.templates["main"]`, func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) { d.template().Synchronization = s }},
		{"template defaults", "spec.templateDefaults", func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			d.wf.Spec.TemplateDefaults = &wfv1.Template{Synchronization: s}
		}},
		{"unused template", `spec.templates["unused"]`, func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			leaf := d.template().DeepCopy()
			leaf.Name, leaf.Synchronization = "unused", s
			d.wf.Spec.Templates = append(d.wf.Spec.Templates, *leaf)
		}},
		{"inline steps", `spec.templates["main"].steps[0]["child"].inline`, func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			leaf := d.template().DeepCopy()
			leaf.Synchronization = s
			*d.template() = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Inline: leaf}}}}}
		}},
		{"inline DAG", `spec.templates["main"].dag.tasks[child].inline`, func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			leaf := d.template().DeepCopy()
			leaf.Synchronization = s
			*d.template() = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: leaf}}}}
		}},
		{"shared template", `templates[shared].templates["work"]`, func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			object := sharedObject("shared")
			object.Spec.Templates[0].Synchronization = s
			d.wf, d.policy.TemplateGetter = sharedConsumer("shared", "work"), getterFor(map[string]*wfv1.WorkflowTemplate{"shared": object})
		}},
		{"shared workflow", "templates[shared].spec", func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			object := sharedObject("shared")
			object.Spec.Synchronization = s
			d.wf, d.policy.TemplateGetter = sharedConsumer("shared", "work"), getterFor(map[string]*wfv1.WorkflowTemplate{"shared": object})
		}},
		{"shared defaults", "templates[shared].spec.templateDefaults", func(_ *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			object := sharedObject("shared")
			object.Spec.TemplateDefaults = &wfv1.Template{Synchronization: s}
			d.wf, d.policy.TemplateGetter = sharedConsumer("shared", "work"), getterFor(map[string]*wfv1.WorkflowTemplate{"shared": object})
		}},
		{"workflow defaults", "spec", func(t *testing.T, d *checkDocument, s *wfv1.Synchronization) {
			raw, err := json.Marshal(&wfv1.Workflow{Spec: wfv1.WorkflowSpec{Synchronization: s}})
			if err != nil {
				t.Fatal(err)
			}
			d.request.Defaults.WorkflowDefaults = raw
			canonical, err := sourcecontract.CanonicalJSON(json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			d.request.Defaults.Digest = sourcecontract.Digest(canonical)
		}},
	}
}

func TestMutexAndSemaphoreNamesAreNotRewritten(t *testing.T) {
	t.Parallel()
	d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: identityHooks()}
	semaphoreDocument(&d)
	d.wf.Spec.Synchronization.Mutexes = []*wfv1.Mutex{{Name: "forge/org/repo/publish", Namespace: "ci"}}
	d.template().Synchronization = &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: "forge/org/repo/build"}}}
	d.sync(t)
	prepared, err := Prepare(t.Context(), d.request, d.policy, d.hooks)
	if err != nil {
		t.Fatal(err)
	}
	w := prepared.View().Workflow
	if !reflect.DeepEqual(w.Spec.Synchronization, d.wf.Spec.Synchronization) || !reflect.DeepEqual(w.Spec.Templates[0].Synchronization, d.template().Synchronization) {
		t.Fatal("synchronization was rewritten")
	}
}

func TestOfflineMutexShapeAndRepositoryBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, repository, lock, code string
	}{
		{"documented example", "", "forge/team/app/publish", ""},
		{"nested lock", "", "forge/team/app/publish/latest", ""},
		{"missing scope", "", "publish", "mutex_scope"},
		{"empty upstream", "", "/team/app/publish", "mutex_scope"},
		{"empty organization", "", "forge//app/publish", "mutex_scope"},
		{"empty repository", "", "forge/team//publish", "mutex_scope"},
		{"empty lock", "", "forge/team/app/", "mutex_scope"},
		{"no lock", "", "forge/team/app", "mutex_scope"},
		{"dynamic scope", "", "{{workflow.parameters.forge}}/team/app/publish", "mutex_scope"},
		{"dynamic lock", "", "forge/team/app/{{workflow.name}}", "mutex_scope"},
		{"known repository", "forge/team/app", "forge/team/app/publish", ""},
		{"foreign repository", "forge/other/app", "forge/team/app/publish", "mutex_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: admissioncontract.PrepareHooks{OfflineValidation: true}}
			d.request.Mode, d.request.Repository = admissioncontract.ModeInspect, tc.repository
			d.request.IdentityRequest = admissioncontract.IdentityRequest{}
			d.wf.Spec.Synchronization = &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: tc.lock}}}
			assertMutexAdmission(t, &d, tc.code, "spec.synchronization.mutexes[0].name")
		})
	}
}

func TestNullSynchronizationEntriesAreRefused(t *testing.T) {
	t.Parallel()
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeSubmit, admissioncontract.ModeInspect} {
		for _, carrier := range mutexCarriers() {
			for kind, synchronization := range map[string]*wfv1.Synchronization{
				"mutexes":    {Mutexes: []*wfv1.Mutex{nil}},
				"semaphores": {Semaphores: []*wfv1.SemaphoreRef{nil}},
			} {
				t.Run(string(mode)+"/"+carrier.name+"/"+kind, func(t *testing.T) {
					d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: identityHooks()}
					d.request.Mode, d.request.IdentityRequest.Mode = mode, mode
					d.hooks.OfflineValidation = mode == admissioncontract.ModeInspect
					if d.hooks.OfflineValidation {
						d.request.Repository = ""
					}
					carrier.set(t, &d, synchronization)
					assertMutexAdmission(t, &d, "sync_null", carrier.path+".synchronization."+kind+"[0]")
				})
			}
		}
	}
}
