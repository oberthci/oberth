package argojob

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
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
		outputs, err := effectiveCredentialOutputs(t.Outputs, wf.Spec.TemplateDefaults)
		if err != nil {
			failure = fmt.Errorf("argojob: template %q effective outputs: %w", t.Name, err)
			return
		}
		otherContainers := len(t.Sidecars) > 0 || (t.ContainerSet != nil && len(t.ContainerSet.Containers) > 1)
		if otherContainers && credentialResultReferenced(wf, t) {
			failure = credentialSharedOutputError(t, "referenced outputs.result")
			return
		}
		paths := []string{}
		for _, p := range outputs.Parameters {
			if p.ValueFrom == nil || p.ValueFrom.Path == "" {
				continue
			}
			if !path.IsAbs(p.ValueFrom.Path) {
				failure = fmt.Errorf("argojob: credentialed template %q output %q read path %q is relative; use an absolute path on the step's own filesystem", t.Name, p.Name, p.ValueFrom.Path)
				return
			}
			if otherContainers {
				failure = credentialSharedOutputError(t, fmt.Sprintf("path-read output parameter %q", p.Name))
				return
			}
			paths = append(paths, p.ValueFrom.Path)
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
	output = path.Clean(output)
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

// Use the same strategic merge schema as Argo's whole-Template default merge.
// Merging by name also preserves inherited nested ValueFrom fields.
func effectiveCredentialOutputs(outputs wfv1.Outputs, defaults *wfv1.Template) (wfv1.Outputs, error) {
	if defaults == nil {
		return outputs, nil
	}
	base, err := json.Marshal(defaults.Outputs)
	if err != nil {
		return wfv1.Outputs{}, err
	}
	leaf, err := json.Marshal(outputs)
	if err != nil {
		return wfv1.Outputs{}, err
	}
	merged, err := strategicpatch.StrategicMergePatch(base, leaf, wfv1.Outputs{})
	if err != nil {
		return wfv1.Outputs{}, err
	}
	var effective wfv1.Outputs
	err = json.Unmarshal(merged, &effective)
	return effective, err
}

func credentialSharedOutputError(t *wfv1.Template, output string) error {
	return fmt.Errorf("argojob: credentialed template %q cannot use %s because another container of the Pod can redirect what Argo reads", t.Name, output)
}

// Argo enables result capture by scanning the invoking parent template for the
// step/task alias. Visit every invocation in the Workflow, including inline
// calls, without confusing aliases reused by unrelated parent templates.
func credentialResultReferenced(wf *wfv1.Workflow, target *wfv1.Template) bool {
	found := false
	walkTemplates(wf, func(parent *wfv1.Template) {
		if found || (parent.DAG == nil && len(parent.Steps) == 0) {
			return
		}
		var metadata *wfv1.Metadata
		for i := range wf.Spec.Templates {
			if parent == &wf.Spec.Templates[i] {
				metadata = wf.Spec.PodMetadata
				break
			}
		}
		body, err := credentialResultScope(parent, wf.Spec.TemplateDefaults, metadata)
		if err != nil {
			found = true
			return
		}
		// JSON escapes double quotes used by expression map indexing.
		text := strings.ReplaceAll(string(body), `\"`, `"`)
		prefix := "steps"
		if parent.DAG != nil {
			prefix = "tasks"
		}
		check := func(name, template string, inline *wfv1.Template) {
			if inline != target && (target.Name == "" || (template != target.Name && !strings.Contains(template, "{{"))) {
				return
			}
			alias := regexp.QuoteMeta(name)
			reference := regexp.MustCompile(prefix + `(?:\.` + alias + `|\[\s*['"]` + alias + `['"]\s*\])\.outputs\.result\b`)
			// Argo's native scan also accepts suffixes and uses an unescaped
			// separator in the indexed form; those still enable wait's read.
			native, _ := regexp.Match(prefix+`\.`+name+`\.outputs\.result`, body)
			nativeIndexed, _ := regexp.Match(prefix+`\[['"]`+name+`['"]\]\.outputs.result`, body)
			found = found || native || nativeIndexed || reference.MatchString(text)
		}
		for _, group := range parent.Steps {
			for _, step := range group.Steps {
				check(step.Name, step.Template, step.Inline)
				for name, hook := range step.Hooks {
					check(string(name), hook.Template, nil)
				}
				check("onExit", step.OnExit, nil)
			}
		}
		if parent.DAG != nil {
			for _, task := range parent.DAG.Tasks {
				check(task.Name, task.Template, task.Inline)
				for name, hook := range task.Hooks {
					check(string(name), hook.Template, nil)
				}
				check("onExit", task.OnExit, nil)
			}
		}
	})
	return found
}

// Resolve workflow Pod metadata as Argo does for named templates, and include
// references inherited through native defaults without modifying the Workflow.
func credentialResultScope(parent, defaults *wfv1.Template, metadata *wfv1.Metadata) ([]byte, error) {
	resolved := parent.DeepCopy()
	if metadata != nil {
		resolved.Metadata.Annotations = map[string]string{}
		maps.Copy(resolved.Metadata.Annotations, metadata.Annotations)
		maps.Copy(resolved.Metadata.Annotations, parent.Metadata.Annotations)
		resolved.Metadata.Labels = map[string]string{}
		maps.Copy(resolved.Metadata.Labels, metadata.Labels)
		maps.Copy(resolved.Metadata.Labels, parent.Metadata.Labels)
	}
	body, err := json.Marshal(resolved)
	if err != nil || defaults == nil {
		return body, err
	}
	applicable := defaults.DeepCopy()
	v := reflect.ValueOf(applicable).Elem()
	for i := 0; i < v.NumField(); i++ {
		field := v.Type().Field(i)
		if field.Type.Kind() != reflect.Pointer || field.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		if t := wfv1.TemplateType(field.Name); t.IsValid() && t != parent.GetType() {
			v.Field(i).Set(reflect.Zero(v.Field(i).Type()))
		}
	}
	base, err := json.Marshal(applicable)
	if err != nil {
		return nil, err
	}
	return strategicpatch.StrategicMergePatch(base, body, wfv1.Template{})
}
