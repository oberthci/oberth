package argoworkflow

import (
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func anyTemplateRef(value reflect.Value, path string, found *[]string) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return
		}
		if value.Type() == reflect.TypeOf(&wfv1.TemplateRef{}) {
			*found = append(*found, path)
			return
		}
		anyTemplateRef(value.Elem(), path, found)
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if !value.Type().Field(index).IsExported() {
				continue
			}
			anyTemplateRef(value.Field(index), path+"."+value.Type().Field(index).Name, found)
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			anyTemplateRef(value.Index(index), path+"[]", found)
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			anyTemplateRef(value.MapIndex(key), path+"[k]", found)
		}
	}
}

func remainingTemplateRefs(t *testing.T, workflow *wfv1.Workflow) []string {
	t.Helper()
	var found []string
	anyTemplateRef(reflect.ValueOf(workflow), "workflow", &found)
	return found
}

func fragmentSource(t *testing.T, templates string) []byte {
	t.Helper()
	return []byte("apiVersion: " + APIVersion + "\nkind: " + Kind +
		"\nspec:\n  entrypoint: main\n  templates:\n" + templates)
}

func stepRefTemplate(name, ref, template string) wfv1.Template {
	return wfv1.Template{
		Name: name,
		Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{
			Name:        "call",
			TemplateRef: &wfv1.TemplateRef{Name: ref, Template: template},
		}}}},
	}
}

func TestParseFragmentRef(t *testing.T) {
	t.Parallel()
	good := map[string]FragmentKey{
		"acme/maven-verify@v3": {Repo: "acme/maven-verify", Version: "v3"},
		"org/repo@v1.2.3":      {Repo: "org/repo", Version: "v1.2.3"},
	}
	for input, want := range good {
		got, err := ParseFragmentRef(input)
		if err != nil {
			t.Fatalf("ParseFragmentRef(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseFragmentRef(%q) = %+v, want %+v", input, got, want)
		}
	}
	bad := []string{
		"",
		"acme/maven-verify",
		"acme/maven-verify@",
		"@v3",
		"a@v1@v2",
		"../escape@v1",
		"/absolute@v1",
		"acme/../../etc@v1",
		"acme/maven-verify@ v3",
	}
	for _, input := range bad {
		if key, err := ParseFragmentRef(input); err == nil {
			t.Fatalf("ParseFragmentRef(%q) admitted %+v, want an error", input, key)
		}
	}
}

func TestFragmentRefsFindsEveryTemplateRefSite(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates: []wfv1.Template{
			stepRefTemplate("from-steps", "org/a@v1", "run"),
			{
				Name: "from-dag",
				DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{
					Name:        "task",
					TemplateRef: &wfv1.TemplateRef{Name: "org/b@v1", Template: "run"},
				}}},
			},
			{
				Name: "from-hook",
				Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{
					Name:     "call",
					Template: "local",
					Hooks: wfv1.LifecycleHooks{"exit": wfv1.LifecycleHook{
						TemplateRef: &wfv1.TemplateRef{Name: "org/c@v1", Template: "run"},
					}},
				}}}},
			},
			{
				Name: "duplicate",
				Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{
					Name:        "again",
					TemplateRef: &wfv1.TemplateRef{Name: "org/a@v1", Template: "other"},
				}}}},
			},
		},
	}}
	refs, err := FragmentRefs(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("FragmentRefs found %d keys (%+v), want 3 deduplicated", len(refs), refs)
	}
	seen := map[string]bool{}
	for _, key := range refs {
		seen[key.String()] = true
	}
	for _, want := range []string{"org/a@v1", "org/b@v1", "org/c@v1"} {
		if !seen[want] {
			t.Fatalf("FragmentRefs missed %q; found %+v", want, refs)
		}
	}
}

func TestFragmentRefsFindsRefsInsideInlineTemplates(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates: []wfv1.Template{{
			Name: "outer",
			Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{
				Name: "wrapper",
				Inline: &wfv1.Template{
					Name: "inner",
					Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{
						Name:        "buried",
						TemplateRef: &wfv1.TemplateRef{Name: "org/deep@v1", Template: "run"},
					}}}},
				},
			}}}},
		}},
	}}
	refs, err := FragmentRefs(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].String() != "org/deep@v1" {
		t.Fatalf("FragmentRefs missed a reference inside an inline template; found %+v", refs)
	}
}

func TestResolveHandlesAWorkflowLevelHookRef(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Hooks: wfv1.LifecycleHooks{"exit": wfv1.LifecycleHook{
			TemplateRef: &wfv1.TemplateRef{Name: "org/notify@v1", Template: "run"},
		}},
		Templates: []wfv1.Template{{Name: "main", Container: &corev1.Container{Image: "golang:1.26"}}},
	}}
	key := FragmentKey{Repo: "org/notify", Version: "v1"}
	fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: "abc", Source: fragmentSource(t,
		"    - name: run\n      container:\n        image: golang:1.26\n")}}

	if _, err := Resolve(workflow, fragments); err != nil {
		t.Fatal(err)
	}
	if remaining := remainingTemplateRefs(t, workflow); len(remaining) != 0 {
		t.Fatalf("templateRef survived at %v", remaining)
	}
	names := map[string]bool{}
	for _, template := range workflow.Spec.Templates {
		names[template.Name] = true
	}
	bound := workflow.Spec.Hooks["exit"].Template
	if bound == "" || !names[bound] {
		t.Fatalf("spec.hooks[exit] names %q, which is not a template in the resolved document", bound)
	}
}

func TestFragmentRefsRefusesMoreThanMaxFragments(t *testing.T) {
	t.Parallel()
	var templates []wfv1.Template
	for index := 0; index <= MaxFragments; index++ {
		suffix := string(rune('a' + index))
		templates = append(templates, stepRefTemplate("t"+suffix, "org/frag"+suffix+"@v1", "run"))
	}
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: templates}}
	_, err := FragmentRefs(workflow)
	if err == nil {
		t.Fatal("FragmentRefs admitted more than MaxFragments references")
	}
	if !strings.Contains(err.Error(), "fragment") {
		t.Fatalf("error does not name the limit it enforces: %v", err)
	}
}

func TestResolveInlinesAndRemovesEveryTemplateRef(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates: []wfv1.Template{
			stepRefTemplate("main", "org/verify@v3", "run"),
			{
				Name: "with-dag",
				DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{
					Name:        "task",
					TemplateRef: &wfv1.TemplateRef{Name: "org/verify@v3", Template: "run"},
				}}},
			},
		},
	}}
	key := FragmentKey{Repo: "org/verify", Version: "v3"}
	fragments := map[FragmentKey]Fragment{key: {
		Key:    key,
		SHA:    "1111111111111111111111111111111111111111",
		Source: fragmentSource(t, "    - name: run\n      container:\n        image: golang:1.26\n"),
	}}

	lock, err := Resolve(workflow, fragments)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := remainingTemplateRefs(t, workflow); len(remaining) != 0 {
		t.Fatalf("templateRef survived resolution at %v; the gate would refuse this document", remaining)
	}
	if len(lock) != 1 || lock[0].Key != key {
		t.Fatalf("lock does not record the one fragment used: %+v", lock)
	}
	if lock[0].Digest == "" {
		t.Fatal("lock records no document digest, so a moved tag would be undetectable")
	}
	names := map[string]bool{}
	for _, template := range workflow.Spec.Templates {
		names[template.Name] = true
	}
	step := workflow.Spec.Templates[0].Steps[0].Steps[0]
	if step.Template == "" || !names[step.Template] {
		t.Fatalf("step names %q, which is not a template in the resolved document", step.Template)
	}
	task := workflow.Spec.Templates[1].DAG.Tasks[0]
	if task.Template == "" || !names[task.Template] {
		t.Fatalf("dag task names %q, which is not a template in the resolved document", task.Template)
	}
}

func TestResolveRewritesReferencesBetweenAFragmentsOwnTemplates(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates:  []wfv1.Template{stepRefTemplate("main", "org/verify@v3", "run")},
	}}
	key := FragmentKey{Repo: "org/verify", Version: "v3"}
	fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: "abc", Source: fragmentSource(t,
		"    - name: run\n"+
			"      steps:\n"+
			"        - - name: inner\n"+
			"            template: helper\n"+
			"    - name: helper\n"+
			"      container:\n"+
			"        image: golang:1.26\n")}}

	if _, err := Resolve(workflow, fragments); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, template := range workflow.Spec.Templates {
		names[template.Name] = true
	}
	var runTemplate *wfv1.Template
	for index := range workflow.Spec.Templates {
		if strings.HasSuffix(workflow.Spec.Templates[index].Name, "run") {
			runTemplate = &workflow.Spec.Templates[index]
		}
	}
	if runTemplate == nil {
		t.Fatalf("the fragment's run template was not inlined; have %v", names)
	}
	called := runTemplate.Steps[0].Steps[0].Template
	if called == "helper" {
		t.Fatal("intra-fragment reference still names the fragment's original template name, which no longer exists")
	}
	if !names[called] {
		t.Fatalf("intra-fragment reference names %q, which is not a template in the resolved document", called)
	}
}

func TestResolveRefusesAFragmentThatItselfReferencesAFragment(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates:  []wfv1.Template{stepRefTemplate("main", "org/outer@v1", "run")},
	}}
	key := FragmentKey{Repo: "org/outer", Version: "v1"}
	fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: "abc", Source: fragmentSource(t,
		"    - name: run\n"+
			"      steps:\n"+
			"        - - name: deeper\n"+
			"            templateRef:\n"+
			"              name: org/inner@v1\n"+
			"              template: run\n")}}

	_, err := Resolve(workflow, fragments)
	if err == nil {
		t.Fatal("a nested fragment was admitted")
	}
	if !strings.Contains(err.Error(), "org/outer@v1") {
		t.Fatalf("error does not name the offending fragment: %v", err)
	}
}

func TestResolveRefusesWorkflowTemplateRef(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint:          "main",
		WorkflowTemplateRef: &wfv1.WorkflowTemplateRef{Name: "org/thing@v1"},
		Templates:           []wfv1.Template{{Name: "main"}},
	}}
	if _, err := Resolve(workflow, map[FragmentKey]Fragment{}); err == nil {
		t.Fatal("spec.workflowTemplateRef was admitted by resolution")
	}
}

func TestResolveRefusesANameThatCollidesWithTheConsumingDocument(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates:  []wfv1.Template{stepRefTemplate("main", "org/verify@v3", "run")},
	}}
	key := FragmentKey{Repo: "org/verify", Version: "v3"}
	fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: "abc", Source: fragmentSource(t,
		"    - name: run\n      container:\n        image: golang:1.26\n")}}
	lock, err := Resolve(workflow, fragments)
	if err != nil {
		t.Fatal(err)
	}
	inlined := ""
	for _, template := range workflow.Spec.Templates {
		if template.Name != "main" {
			inlined = template.Name
		}
	}
	if inlined == "" {
		t.Fatalf("nothing was inlined; lock %+v", lock)
	}

	colliding := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates: []wfv1.Template{
			stepRefTemplate("main", "org/verify@v3", "run"),
			{Name: inlined},
		},
	}}
	if _, err := Resolve(colliding, fragments); err == nil {
		t.Fatalf("resolution silently overwrote or duplicated the template name %q", inlined)
	}
}

func TestResolveKeepsTwoFragmentsApart(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates: []wfv1.Template{
			stepRefTemplate("first", "org/a@v1", "run"),
			stepRefTemplate("second", "org/b@v1", "run"),
		},
	}}
	body := "    - name: run\n      container:\n        image: golang:1.26\n"
	keyA := FragmentKey{Repo: "org/a", Version: "v1"}
	keyB := FragmentKey{Repo: "org/b", Version: "v1"}
	fragments := map[FragmentKey]Fragment{
		keyA: {Key: keyA, SHA: "aaa", Source: fragmentSource(t, body)},
		keyB: {Key: keyB, SHA: "bbb", Source: fragmentSource(t, body)},
	}
	if _, err := Resolve(workflow, fragments); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, template := range workflow.Spec.Templates {
		if seen[template.Name] {
			t.Fatalf("duplicate template name %q after resolving two fragments", template.Name)
		}
		seen[template.Name] = true
	}
	if len(workflow.Spec.Templates) != 4 {
		t.Fatalf("expected two originals plus one template from each fragment, got %d", len(workflow.Spec.Templates))
	}
}

func TestResolveRefusesAReferenceMissingFromTheMap(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates:  []wfv1.Template{stepRefTemplate("main", "org/absent@v9", "run")},
	}}
	_, err := Resolve(workflow, map[FragmentKey]Fragment{})
	if err == nil {
		t.Fatal("a missing fragment resolved successfully")
	}
	if !strings.Contains(err.Error(), "org/absent@v9") {
		t.Fatalf("error does not name the missing fragment: %v", err)
	}
	if remaining := remainingTemplateRefs(t, workflow); len(remaining) == 0 {
		t.Fatal("the document was mutated despite the failure; resolution must be all or nothing")
	}
}

func TestResolveRefusesAFragmentNamingATemplateItDoesNotHave(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates:  []wfv1.Template{stepRefTemplate("main", "org/verify@v3", "absent")},
	}}
	key := FragmentKey{Repo: "org/verify", Version: "v3"}
	fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: "abc", Source: fragmentSource(t,
		"    - name: run\n      container:\n        image: golang:1.26\n")}}
	if _, err := Resolve(workflow, fragments); err == nil {
		t.Fatal("a reference to a template the fragment does not export resolved successfully")
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	t.Parallel()
	key := FragmentKey{Repo: "org/verify", Version: "v3"}
	fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: "abc", Source: fragmentSource(t,
		"    - name: run\n      container:\n        image: golang:1.26\n"+
			"    - name: other\n      container:\n        image: golang:1.26\n")}}

	build := func() (*wfv1.Workflow, Lock) {
		workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
			Entrypoint: "main",
			Templates:  []wfv1.Template{stepRefTemplate("main", "org/verify@v3", "run")},
		}}
		lock, err := Resolve(workflow, fragments)
		if err != nil {
			t.Fatal(err)
		}
		return workflow, lock
	}
	firstWorkflow, firstLock := build()
	secondWorkflow, secondLock := build()
	if renderJSON(t, firstWorkflow) != renderJSON(t, secondWorkflow) {
		t.Fatal("two resolutions of the same input produced different documents")
	}
	if renderJSON(t, firstLock) != renderJSON(t, secondLock) {
		t.Fatal("two resolutions of the same input produced different locks")
	}
}

func TestResolveLeavesADocumentWithNoFragmentsAlone(t *testing.T) {
	t.Parallel()
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "main",
		Templates: []wfv1.Template{{
			Name: "main",
			Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{
				Name: "call", Template: "worker",
			}}}},
		}, {
			Name:      "worker",
			Container: &corev1.Container{Image: "golang:1.26"},
		}},
	}}
	before := renderJSON(t, workflow)
	lock, err := Resolve(workflow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock) != 0 {
		t.Fatalf("a document with no fragments produced a non-empty lock: %+v", lock)
	}
	if renderJSON(t, workflow) != before {
		t.Fatal("Resolve mutated a document that references no fragments")
	}
}
