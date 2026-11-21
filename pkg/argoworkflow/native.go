package argoworkflow

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// Native validation increments a process-wide placeholder counter.
//
//nolint:gochecknoglobals // A cancellable gate serializes every native validation call.
var nativeSchemaGate = make(chan struct{}, 1)

type retainedTemplates map[string]*wfv1.WorkflowTemplate

func (r retainedTemplates) Get(ctx context.Context, name string) (*wfv1.WorkflowTemplate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r[name] == nil {
		return nil, refusal("templates["+name+"]", "reference")
	}
	return r[name].DeepCopy(), nil
}

// ValidateNativeSchema checks the pinned native semantics using retained objects
// only. SelectedFile supplies the kind; an empty file keeps it unknown.
// It never fetches live templates or transforms a sealed result.
func ValidateNativeSchema(ctx context.Context, wf *wfv1.Workflow, request admissioncontract.PrepareRequest) error {
	request.Mode = admissioncontract.ModeInspect
	return validateNativeSchema(ctx, wf, request, nil)
}

func validateNativeSchema(ctx context.Context, wf *wfv1.Workflow, request admissioncontract.PrepareRequest, g *graph) error {
	select {
	case nativeSchemaGate <- struct{}{}:
		defer func() { <-nativeSchemaGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if wf == nil {
		return refusal("workflow", "document")
	}
	objects := retainedTemplates{}
	for _, s := range request.Templates {
		var spec wfv1.WorkflowSpec
		if json.Unmarshal(s.Spec, &spec) != nil {
			return refusal("templates["+s.Name+"]", "native")
		}
		obj := &wfv1.WorkflowTemplate{Spec: spec}
		obj.Name = s.Name
		obj.Namespace = s.Namespace
		objects[s.Name] = obj
	}
	logger := logging.NewSlogLoggerCustom(logging.Error, logging.Text, io.Discard)
	nativeContext := logging.WithLogger(ctx, logger)
	if g == nil {
		selected, release, err := workflowpolicy.Select(nil, workflowpolicy.SelectionRequest{File: request.SelectedFile})
		if err != nil {
			return err
		}
		request.SelectedFile = selected
		g = newGraph(ctx, wf, request, Policy{TemplateGetter: objects.Get, Release: release})
		g.build()
		if err := g.err(); err != nil {
			return err
		}
	}
	return nativeFindings(nativeContext, wf, objects, g)
}

func nativeErrorPath(detail string) string {
	token := strings.Fields(detail)
	if len(token) == 0 {
		return "spec"
	}
	path := strings.TrimSuffix(token[0], ":")
	for _, part := range token[1:] {
		if strings.HasPrefix(part, "templates.") && len(part) > len(path) && strings.HasPrefix(part, path+".") {
			path = strings.TrimSuffix(part, ":")
		}
	}
	if strings.HasPrefix(path, "hooks.") {
		path = "spec." + path
	}
	if !strings.HasPrefix(path, "spec.") && !strings.HasPrefix(path, "templates.") {
		return "spec"
	}
	path = regexp.MustCompile(`^templates\.([a-zA-Z0-9_-]+)`).ReplaceAllString(path, `spec.templates["$1"]`)
	for _, part := range []struct{ from, to string }{{"tasks", "dag.tasks"}, {"steps", "steps"}, {"parameters", "parameters"}, {"artifacts", "artifacts"}, {"hooks", "hooks"}} {
		pattern := regexp.MustCompile(`\.` + part.from + `\.([a-zA-Z0-9_-]+)`)
		path = pattern.ReplaceAllString(path, "."+part.to+"[$1]")
	}
	path = regexp.MustCompile(`\.arguments\.([a-zA-Z0-9_-]+)\.(value|valueFrom)`).ReplaceAllString(path, `.arguments.parameters[$1].$2`)
	path = regexp.MustCompile(`\.steps\[([0-9]+)\]\.([a-zA-Z0-9_-]+)`).ReplaceAllString(path, `.steps[$1]["$2"]`)
	return path
}
