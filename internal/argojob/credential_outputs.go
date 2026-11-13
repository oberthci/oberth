package argojob

import (
	"fmt"
	"path"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// Argo wait follows symlinks when reading mounted output paths. An earlier
// ungranted step can plant a link into a credential mirror on a shared claim.
// Check after all mounts are injected, including inherited output declarations.
func validateCredentialOutputPaths(wf *wfv1.Workflow, granted map[*wfv1.Template]bool, cloud []*wfv1.Template) error {
	credentialed := make(map[*wfv1.Template]bool, len(granted)+len(cloud))
	for t, allowed := range granted {
		credentialed[t] = allowed
	}
	for _, t := range cloud {
		credentialed[t] = true
	}
	var failure error
	walkTemplates(wf, func(t *wfv1.Template) {
		if !credentialed[t] || failure != nil {
			return
		}
		outputs := t.Outputs
		if defaults := wf.Spec.TemplateDefaults; defaults != nil {
			outputs.Parameters = append(append([]wfv1.Parameter(nil), outputs.Parameters...), defaults.Outputs.Parameters...)
			outputs.Artifacts = append(append(wfv1.Artifacts(nil), outputs.Artifacts...), defaults.Outputs.Artifacts...)
		}
		paths := []string{}
		for _, p := range outputs.Parameters {
			if p.ValueFrom != nil && p.ValueFrom.Path != "" {
				paths = append(paths, p.ValueFrom.Path)
			}
		}
		for _, a := range outputs.Artifacts {
			if a.Path != "" {
				paths = append(paths, a.Path)
			}
		}
		for _, output := range paths {
			if credentialOutputOnMount(t, output) {
				failure = fmt.Errorf("argojob: credentialed template %q output read path %q is on a volume mount or cannot be resolved before execution", t.Name, output)
				return
			}
		}
	})
	return failure
}

func credentialOutputOnMount(t *wfv1.Template, output string) bool {
	// Substitution can move a path onto any mount after admission.
	if strings.Contains(output, "{{") {
		return true
	}
	output = path.Clean("/" + output)
	overlaps := false
	templateContainers(t, func(c *corev1.Container) {
		for _, m := range c.VolumeMounts {
			mount := path.Clean("/" + m.MountPath)
			if strings.Contains(m.MountPath, "{{") || mount == "/" || output == mount || strings.HasPrefix(output, mount+"/") {
				overlaps = true
			}
		}
	})
	// Argo can also read from a shared input-artifact volume in wait.
	for _, a := range t.Inputs.Artifacts {
		mount := path.Clean("/" + a.Path)
		if a.Path != "" && (output == mount || strings.HasPrefix(output, mount+"/")) {
			overlaps = true
		}
	}
	return overlaps
}
