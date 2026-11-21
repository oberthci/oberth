package argoworkflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
)

func TestReservedMountShapes(t *testing.T) {
	t.Parallel()
	for _, root := range ReservedMountRoots() {
		for _, tc := range []struct {
			name, value string
			refused     bool
		}{
			{"equal", root, true}, {"child", root + "/file", true},
			{"parent", root[:strings.LastIndex(root, "/")], true},
			{"sibling", root + "2", false}, {"trailing", root + "/", true},
			{"double", strings.ReplaceAll(root, "/", "//"), true},
			{"var-prefix", "/var" + root, strings.HasPrefix(root, "/run/")},
		} {
			t.Run(root+"/"+tc.name, func(t *testing.T) {
				w, p := mountDocument("container", tc.value)
				_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
				if !tc.refused {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				assertMountRefusal(t, err, tc.value)
			})
		}
	}
}

func TestReservedMountContainerKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "init", "sidecar", "member", "set", "defaults", "unused", "inline", "reference"} {
		t.Run(kind, func(t *testing.T) {
			for _, mount := range []string{"/safe", "/var/run/argo/socket", "/argo/staging"} {
				w, p := mountDocument(kind, mount)
				_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
				if mount == "/safe" {
					if err != nil {
						t.Fatal("positive control", err)
					}
					continue
				}
				assertMountRefusal(t, err, mount)
			}
		})
	}
}

func TestMountPathSyntax(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"/work/../data", "/../data", "/data/..", "/work//../data/", "relative", "/{{inputs.parameters.path}}"} {
		t.Run(value, func(t *testing.T) {
			w, p := mountDocument("container", value)
			_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
			assertMountRefusal(t, err, value)
		})
	}
	for _, value := range []string{"/run/identity", "/run/identity/token", "/var/run/argo2", "/safe//data/", "/safe/./data", "/safe/..data"} {
		t.Run(value, func(t *testing.T) {
			w, p := mountDocument("container", value)
			if _, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGrantedLoginMountCannotTraverse(t *testing.T) {
	t.Parallel()
	w, f := tokenWorkflow()
	w.Spec.Templates[0].Container.VolumeMounts[0].MountPath = "/var/run/secrets/elsewhere/../secretstore"
	requireCode(t, admitFixture(t.Context(), w, f), "path")
}

func TestMountRefusalPreservesLongStepAndPath(t *testing.T) {
	t.Parallel()
	value := "/run/argo/" + strings.Repeat("segment/", 100)
	w, p := mountDocument("container", value)
	name := strings.Repeat("s", 60)
	w.Spec.Entrypoint, w.Spec.Templates[0].Name = name, name
	_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
	assertMountRefusal(t, err, value)
	if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), value) {
		t.Fatal("step or path truncated", err)
	}
}

func TestReservedMountRootsHaveNoMutableAlias(t *testing.T) {
	t.Parallel()
	roots := ReservedMountRoots()
	want := slices.Clone(roots)
	roots[0] = "/authored"
	if !slices.Equal(ReservedMountRoots(), want) {
		t.Fatal("caller changed the reserved roots")
	}
}

func assertMountRefusal(t *testing.T, err error, value string) {
	t.Helper()
	for _, f := range admissionFindings(t, err) {
		if f.Code != "path" {
			continue
		}
		if !strings.Contains(f.Path, fmt.Sprintf("[%q]", value)) || !strings.Contains(f.Path, "templates[") && !strings.Contains(f.Path, "templateDefaults") {
			t.Fatal("step/path missing", f)
		}
		if len(f.Reason) > 80 || len(f.Remedy) > 120 || strings.Contains(f.Reason+f.Remedy, "…") || strings.Contains(f.Reason+f.Remedy, "...") {
			t.Fatal("unbounded message", f)
		}
		if !strings.Contains(f.Remedy, "somewhere else") {
			t.Fatal("missing remedy", f)
		}
		return
	}
	t.Fatal("missing path refusal", err)
}

func mountDocument(kind, value string) (*wf.Workflow, Policy) {
	return mountSpecDocument(kind, core.VolumeMount{Name: "data", MountPath: value})
}

func mountSpecDocument(kind string, mount core.VolumeMount) (*wf.Workflow, Policy) {
	w := simpleWorkflow()
	p := Policy{}
	leaf := &w.Spec.Templates[0]
	if kind == "reference" {
		w = sharedConsumer("shared", "work")
		shared := sharedObject("shared")
		leaf = &shared.Spec.Templates[0]
		p.TemplateGetter = getterFor(map[string]*wf.WorkflowTemplate{"shared": shared})
	}
	leaf.Volumes = []core.Volume{{Name: "data", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
	container := core.Container{Name: "worker", Image: "example.test/image:1", Command: []string{"true"}, VolumeMounts: []core.VolumeMount{mount}}
	switch kind {
	case "script":
		leaf.Container = nil
		leaf.Script = &wf.ScriptTemplate{Container: container, Source: "true"}
	case "init":
		leaf.InitContainers = []wf.UserContainer{{Container: container}}
	case "sidecar":
		leaf.Sidecars = []wf.UserContainer{{Container: container}}
	case "member", "set":
		leaf.Container = nil
		leaf.ContainerSet = &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: container}}}
		if kind == "set" {
			leaf.ContainerSet.Containers[0].VolumeMounts = nil
			leaf.ContainerSet.VolumeMounts = []core.VolumeMount{mount}
		}
	case "defaults":
		w.Spec.TemplateDefaults = &wf.Template{Container: &container}
	case "unused":
		leaf = &wf.Template{Name: "unused", Container: &container, Volumes: leaf.Volumes}
		w.Spec.Templates = append(w.Spec.Templates, *leaf)
	case "inline":
		child := leaf.DeepCopy()
		child.Container = &container
		*leaf = wf.Template{Name: "main", Steps: []wf.ParallelSteps{{Steps: []wf.WorkflowStep{{Name: "call", Inline: child}}}}}
	default:
		leaf.Container = &container
	}
	return w, p
}
