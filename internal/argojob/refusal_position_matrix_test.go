package argojob

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/validate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const refusalImg = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func refusalBuild(t *testing.T, cfg Config, trigger pipeline.Trigger, w *wfv1.Workflow) (*wfv1.Workflow, string) {
	t.Helper()
	w.APIVersion, w.Kind = "argoproj.io/v1alpha1", "Workflow"
	src, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		w *wfv1.Workflow
		s string
	}
	done := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- res{nil, fmt.Sprintf("PANIC %v", r)}
			}
		}()
		r := testRequest(trigger, string(src))
		r.UpstreamName = "upstream"
		out, err := Build(cfg, r)
		if err != nil {
			done <- res{nil, "err: " + err.Error()}
			return
		}
		done <- res{out, "ok"}
	}()
	select {
	case r := <-done:
		return r.w, r.s
	case <-time.After(20 * time.Second):
		return nil, "TIMEOUT"
	}
}

func refusalLeafT(n string) wfv1.Template {
	return wfv1.Template{Name: n, Container: &corev1.Container{Image: refusalImg, Command: []string{"/bin/true"}}}
}

func TestRefusalPositionBuildDupRows(t *testing.T) {
	neg := resource.MustParse("-1")
	docs := map[string]*wfv1.Workflow{
		"zero":             {},
		"no-entrypoint":    {Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{refusalLeafT("main")}}},
		"no-templates":     {Spec: wfv1.WorkflowSpec{Entrypoint: "main"}},
		"dup-names":        {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{refusalLeafT("main"), refusalLeafT("main")}}},
		"empty-name":       {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{refusalLeafT("main"), refusalLeafT(""), refusalLeafT("")}}},
		"undeclared-entry": {Spec: wfv1.WorkflowSpec{Entrypoint: "ghost", Templates: []wfv1.Template{refusalLeafT("main")}}},
		"undeclared-ref": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", OnExit: "ghost", Hooks: wfv1.LifecycleHooks{"x": {Template: "ghost"}}, Templates: []wfv1.Template{
			{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "s", Template: "ghost", Hooks: wfv1.LifecycleHooks{"x": {Template: "ghost2"}}}, {Name: "", Template: ""}}}}},
			{Name: "d", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "t", Template: "ghost", Depends: "nope", Dependencies: []string{"nope2"}}}}}}}},
		"self-recursion": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "main"}, {Name: "l", Template: "leaf"}}}}}, refusalLeafT("leaf")}}},
		"mutual-recursion": {Spec: wfv1.WorkflowSpec{Entrypoint: "a", Templates: []wfv1.Template{
			{Name: "a", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "x", Template: "b"}, {Name: "l", Template: "leaf"}}}},
			{Name: "b", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "y", Template: "a"}, {Name: "l2", Template: "leaf"}}}}}, refusalLeafT("leaf")}}},
		"negative-resources": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template {
			x := refusalLeafT("main")
			x.Container.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": neg, "memory": resource.MustParse("10Gi")}, Limits: corev1.ResourceList{"memory": resource.MustParse("1Mi")}}
			return x
		}()}}},
		"relative-mount": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}, Templates: []wfv1.Template{func() wfv1.Template {
			x := refusalLeafT("main")
			x.Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "rel/x"}, {Name: "v", MountPath: ""}}
			return x
		}()}}},
		"unknown-volume": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template {
			x := refusalLeafT("main")
			x.Container.VolumeMounts = []corev1.VolumeMount{{Name: "ghost", MountPath: "/x"}}
			return x
		}()}}},
		"unknown-volume-set": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: refusalImg, VolumeMounts: []corev1.VolumeMount{{Name: "ghost", MountPath: "/x"}}}}}}}}}},
		"nil-pieces":         {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{}}, {Name: "s", Steps: []wfv1.ParallelSteps{{}}}, {Name: "cs", ContainerSet: &wfv1.ContainerSetTemplate{}}, {Name: "nothing"}}}},
		"camel-weird-names": {Spec: wfv1.WorkflowSpec{Entrypoint: "Main_Thing", Templates: []wfv1.Template{
			{Name: "Main_Thing", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "A/b ../x", Template: "../../etc/passwd"}, {Name: "n\nl", Template: strings.Repeat("X", 5000)}}}}},
			refusalLeafT("../../etc/passwd"), refusalLeafT(strings.Repeat("X", 5000)), refusalLeafT("{{workflow.parameters.x}}")}}},
		"empty-image": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template { x := refusalLeafT("main"); x.Container.Image = ""; return x }()}}},
		"templated-image": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template {
			x := refusalLeafT("main")
			x.Container.Image = "{{workflow.parameters.i}}"
			return x
		}()}}},
		"garbage-image": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template {
			x := refusalLeafT("main")
			x.Container.Image = "@@::// \n" + strings.Repeat("a", 4000)
			return x
		}()}}},
	}
	for name, w := range docs {
		for _, trig := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
			t.Run(name+"/"+string(trig), func(t *testing.T) {
				out, s := refusalBuild(t, testConfig(), trig, w.DeepCopy())
				if strings.HasPrefix(s, "PANIC") || s == "TIMEOUT" {
					t.Errorf("%s", s)
				}
				if len(s) > 400 {
					s = s[:400]
				}
				t.Logf("%s", s)
				_ = out
			})
		}
	}
}

func TestRefusalPositionBuildAdmitted(t *testing.T) {
	// generateName
	w := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{refusalLeafT("main")}}}
	w.GenerateName = "evil-"
	out, s := refusalBuild(t, testConfig(), pipeline.TriggerCI, w)
	if out == nil || out.GenerateName != "" || out.Name != "oberth-oberth-run-abc1-aabbccddeeff" {
		t.Errorf("generateName: %s %+v", s, out)
	}
	// imagePullSecrets
	w = &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}}, Templates: []wfv1.Template{refusalLeafT("main")}}}
	out, s = refusalBuild(t, testConfig(), pipeline.TriggerCI, w)
	if out == nil || len(out.Spec.ImagePullSecrets) != 1 || out.Spec.ImagePullSecrets[0].Name != "regcred" {
		t.Fatalf("imagePullSecrets not preserved: %s", s)
	}
	// allowlist honoured
	cfg := testConfig()
	cfg.RunnerImagePrefixes = []string{"golang:"}
	for img, want := range map[string]bool{"golang:1.26": true, refusalImg: true, "golang": false, "evil.io/golang:1": false, "debian:12": false, "": false, "{{workflow.parameters.i}}": false, "golang:{{inputs.parameters.t}}": false, "index.docker.io/library/golang:1": true, "docker.io/evil/golang:1": false, "golang:1@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": true} {
		for _, form := range []string{"container", "script", "init", "sidecar", "set", "inline", "defaults"} {
			w := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{refusalLeafT("main")}}}
			c := corev1.Container{Name: "x", Image: img, Command: []string{"true"}}
			leaf := &w.Spec.Templates[0]
			switch form {
			case "container":
				c.Name = ""
				leaf.Container = &c
			case "script":
				c.Name = ""
				leaf.Container = nil
				leaf.Script = &wfv1.ScriptTemplate{Container: c, Source: "true"}
			case "init":
				leaf.InitContainers = []wfv1.UserContainer{{Container: c}}
			case "sidecar":
				leaf.Sidecars = []wfv1.UserContainer{{Container: c}}
			case "set":
				c.Name = "main"
				leaf.Container = nil
				leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: c}}}
			case "inline":
				c.Name = ""
				w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: &wfv1.Template{Container: &c}}}}}}
			case "defaults":
				c.Name = ""
				w.Spec.TemplateDefaults = &wfv1.Template{Container: &c}
			}
			err := argoworkflow.Admit(w, argoworkflow.Policy{RunnerImagePrefixes: cfg.RunnerImagePrefixes})
			refusedForImage := err != nil && strings.Contains(err.Error(), "allowlist")
			if want == refusedForImage {
				t.Errorf("allowlist image %q form %s: want allowed=%v err=%v", img, form, want, err)
			}
		}
	}
}

func TestRefusalPositionWIFStamp(t *testing.T) {
	const key = "oberth.ci/release-wif-policy-sha256"
	// honest
	out, err := Build(wifTestConfig(), wifTestRequest(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	honest := out.Annotations[key]
	if len(honest) != 64 {
		t.Fatalf("no stamp: %q", honest)
	}
	// author pre-supplies with a WIF leaf
	_, err = Build(wifTestConfig(), wifTestRequest(t, func(w *wfv1.Workflow) { w.Annotations[key] = strings.Repeat("0", 64) }))
	t.Logf("presupplied with leaf: %v", err)
	if err == nil {
		t.Errorf("author provenance with WIF leaf ADMITTED")
	}
	// author pre-supplies without a WIF leaf
	_, err = Build(wifTestConfig(), wifTestRequest(t, func(w *wfv1.Workflow) {
		w.Annotations[key] = honest
		w.Spec.Templates[0].Metadata.Annotations = nil
	}))
	t.Logf("presupplied without leaf: %v", err)
	if err == nil {
		t.Errorf("author provenance without WIF leaf ADMITTED")
	}
	// via workflowMetadata (controller copies onto live Workflow)
	_, err = Build(wifTestConfig(), wifTestRequest(t, func(w *wfv1.Workflow) {
		w.Spec.WorkflowMetadata = &wfv1.WorkflowMetadata{Annotations: map[string]string{key: strings.Repeat("0", 64)}}
	}))
	if err == nil {
		t.Errorf("provenance via workflowMetadata ADMITTED")
	}
	// label carrying same key
	_, err = Build(wifTestConfig(), wifTestRequest(t, func(w *wfv1.Workflow) { w.Labels = map[string]string{key: "x"} }))
	if err == nil {
		t.Errorf("provenance label ADMITTED")
	}
	// WIF authorisation checks still run
	type m = func(*wfv1.Workflow)
	for name, mut := range map[string]m{
		"unapproved-role": func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "chart-writer"
		},
		"workflow-level": func(w *wfv1.Workflow) { w.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] = "image-writer" },
		"defaults": func(w *wfv1.Workflow) {
			w.Spec.TemplateDefaults = &wfv1.Template{Metadata: wfv1.Metadata{Annotations: map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}}}
		},
		"other-template": func(w *wfv1.Workflow) {
			x := refusalLeafT("other")
			x.Metadata.Annotations = map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}
			w.Spec.Templates = append(w.Spec.Templates, x)
		},
		"inline-named-main": func(w *wfv1.Workflow) {
			x := refusalLeafT("main")
			x.Metadata.Annotations = map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}
			w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: "d", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: &x}}}})
		},
		"volume-alias": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: releaseWIFVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		},
		"mount-path": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/run/oberth-gar"}}
		},
		"mount-path-REL": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "run/oberth-gar"}}
		},
		"helper-path-REL": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			w.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "run/oberth/bin"}}
		},
	} {
		_, err := Build(wifTestConfig(), wifTestRequest(t, mut))
		if err == nil {
			t.Errorf("WIF %s ADMITTED", name)
		} else {
			t.Logf("WIF %s refused: %.120s", name, err.Error())
		}
	}
	// CI trigger must not get WIF
	r := wifTestRequest(t, nil)
	r.Trigger = pipeline.TriggerCI
	if _, err := Build(wifTestConfig(), r); err == nil {
		t.Errorf("WIF on CI ADMITTED")
	}
}

// Duplicate names remain Argo validation, including on an approved WIF leaf.
func TestRefusalPositionDuplicateWIFNamesUseNativeValidation(t *testing.T) {
	request := wifTestRequest(t, func(w *wfv1.Workflow) {
		duplicate := refusalLeafT("main")
		duplicate.Metadata.Annotations = map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}
		w.Spec.Templates = append(w.Spec.Templates, duplicate)
	})
	workflow, err := Build(wifTestConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	err = validate.ValidateWorkflow(logging.TestContext(t.Context()), nil, nil, workflow, nil, validate.ValidateOpts{Submit: true})
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("native duplicate-name refusal: %v", err)
	}
}
