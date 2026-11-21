package argoworkflow

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
)

func TestAuthoredContainersCannotUseArtifactInitNames(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "member", "init", "sidecar"} {
		for _, name := range []string{"init-artifact-oberth", "init-artifact-custom", "init-artifact-", "init-artifacts-custom", "worker"} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				w := simpleWorkflow()
				tpl := &w.Spec.Templates[0]
				c := core.Container{Name: name, Image: "example.test/image:1", Command: []string{"true"}}
				switch kind {
				case "container":
					tpl.Container = &c
				case "script":
					tpl.Container = nil
					tpl.Script = &wf.ScriptTemplate{Container: c, Source: "true"}
				case "member":
					tpl.Container = nil
					tpl.ContainerSet = &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: c}}}
				case "init":
					tpl.InitContainers = []wf.UserContainer{{Container: c}}
				case "sidecar":
					tpl.Sidecars = []wf.UserContainer{{Container: c}}
				}
				_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
				if name == "worker" || name == "init-artifacts-custom" {
					if err != nil {
						t.Fatal("ordinary container name refused", err)
					}
				} else {
					requireCode(t, err, "reserved")
				}
			})
		}
	}
}
