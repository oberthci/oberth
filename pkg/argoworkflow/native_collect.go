package argoworkflow

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/templateresolution"
	"github.com/argoproj/argo-workflows/v4/workflow/validate"
	"github.com/oberthci/oberth/internal/admissioncontract"
	core "k8s.io/api/core/v1"
)

const maxNativeFindings = 20

type nativeCollector struct {
	result      AdmissionError
	diagnostics nativeDiagnostics
	seen        map[admissioncontract.Finding]bool
	work        int
	faults      int
	stopped     bool
}

// Argo returns one error. Continue on disposable diagnostic copies after masking
// the reported value or template, up to a bounded number of findings per request.
// The admitted object is never repaired or sealed.
func (c *nativeCollector) scope(ctx context.Context, candidate *wf.Workflow, objects retainedTemplates, group []*node) error {
	ignore := candidate.Spec.Entrypoint == ""
	for !c.stopped {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, _ := json.Marshal(candidate)
		c.work += max(1, len(raw)/1024)
		if c.work > maxInspectionNodes {
			c.stop("native_work")
			return nil
		}
		err := validate.ValidateWorkflow(ctx, objects, &templateresolution.NullClusterWorkflowTemplateGetter{}, candidate, nil, validate.ValidateOpts{Submit: true, IgnoreEntrypoint: ignore})
		if err == nil {
			return nil
		}
		if c.faults == maxNativeFindings {
			c.stop("native_more")
			return nil
		}
		c.faults++
		if c.result.cause == nil {
			c.result.cause = err
		}
		if c.unresolved(candidate, err.Error(), group[0].scope) {
			continue
		}
		location := c.diagnostics.location(group[0].scope, err.Error())
		c.add(finding(location, "native"), err.Error())
		if maskNativeArgument(candidate, location) {
			continue
		}
		if maskNativeHook(candidate, location) {
			continue
		}
		if !muteNativeTemplate(candidate, location, group) {
			return nil
		}
	}
	return nil
}

func maskNativeHook(candidate *wf.Workflow, location string) bool {
	name, ok := strings.CutPrefix(location, "spec.hooks[")
	if !ok {
		return false
	}
	name, _, ok = strings.Cut(name, "]")
	if !ok {
		return false
	}
	key := wf.LifecycleEvent(name)
	hook, exists := candidate.Spec.Hooks[key]
	if !exists || hook.Expression != "" {
		return false
	}
	hook.Expression = "true"
	candidate.Spec.Hooks[key] = hook
	return true
}

func (c *nativeCollector) add(f admissioncontract.Finding, detail string) {
	if c.stopped || c.seen[f] {
		return
	}
	if len(c.result.Findings) == maxNativeFindings {
		c.stop("native_more")
		return
	}
	c.seen[f] = true
	c.result.Findings = append(c.result.Findings, f)
	c.result.Detail += f.Path + ": " + detail + c.diagnostics.callersFor(f.Path) + "\n"
}

func (c *nativeCollector) stop(code string) {
	c.stopped = true
	c.result.Findings = append(c.result.Findings, finding("native_schema", code))
}

func (c *nativeCollector) unresolved(candidate *wf.Workflow, detail, scope string) bool {
	match := regexp.MustCompile(`failed to resolve (\{\{[^{}]+\}\})`).FindStringSubmatch(detail)
	if len(match) == 0 {
		return false
	}
	raw, _ := json.Marshal(candidate)
	changed := false
	location := c.diagnostics.location(scope, detail)
	raw = walkStrings(raw, func(path, value string) string {
		if scope != "" {
			path = "templates[" + scope + "]." + strings.TrimPrefix(path, "spec.")
		}
		if !strings.Contains(value, match[1]) || !strings.HasPrefix(path, location+".") && path != location {
			return value
		}
		c.add(unresolvedFinding(path, match[1]), detail)
		changed = true
		return strings.ReplaceAll(value, match[1], "unverified")
	})
	if changed {
		_ = json.Unmarshal(raw, candidate)
	}
	return changed
}

func muteNativeTemplate(w *wf.Workflow, location string, group []*node) bool {
	for _, n := range group {
		if n.inlineDepth != 0 || location != n.path && !strings.HasPrefix(location, n.path+".") {
			continue
		}
		for i := range w.Spec.Templates {
			t := &w.Spec.Templates[i]
			if t.Name != n.template.Name || t.Container != nil && t.Container.Image == "native-diagnostic" {
				continue
			}
			*t = nativePlaceholder(*t)
			return true
		}
	}
	return false
}

func nativePlaceholder(t wf.Template) wf.Template {
	stub := wf.Template{Name: t.Name, Inputs: *t.Inputs.DeepCopy(), Outputs: *t.Outputs.DeepCopy(), Container: &core.Container{Image: "native-diagnostic"}}
	for i, p := range stub.Outputs.Parameters {
		stub.Outputs.Parameters[i] = wf.Parameter{Name: p.Name, GlobalName: p.GlobalName, Value: wf.AnyStringPtr("unverified")}
	}
	for i, a := range stub.Outputs.Artifacts {
		stub.Outputs.Artifacts[i] = wf.Artifact{Name: a.Name, GlobalName: a.GlobalName, Path: "/native-diagnostic"}
	}
	return stub
}
