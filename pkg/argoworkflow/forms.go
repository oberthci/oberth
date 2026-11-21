package argoworkflow

import (
	"reflect"
	"slices"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// AdmittedTemplateForms is the constructor set checked by admission.
func AdmittedTemplateForms() []wf.TemplateType {
	return []wf.TemplateType{wf.TemplateTypeContainer, wf.TemplateTypeScript, wf.TemplateTypeContainerSet, wf.TemplateTypeResource, wf.TemplateTypeSuspend, wf.TemplateTypeDAG, wf.TemplateTypeSteps}
}

func (g *graph) templateForm(n *node) {
	value := reflect.ValueOf(n.template).Elem()
	for i := range value.NumField() {
		kind := wf.TemplateType(value.Type().Field(i).Name)
		if !kind.IsValid() || value.Field(i).IsZero() || slices.Contains(AdmittedTemplateForms(), kind) {
			continue
		}
		code := "native"
		switch kind {
		case wf.TemplateTypeHTTP:
			code = "http"
		case wf.TemplateTypePlugin:
			code = "plugin"
		case wf.TemplateTypeData:
			code = "read"
		}
		g.refuse(n.path+"."+strings.ToLower(string(kind)), code)
	}
}

const (
	namedInvocation = iota
	inlineInvocation
	referenceInvocation
)

// AdmittedInvocationForms enumerates the targets produced by invocationForm.
func AdmittedInvocationForms() []string {
	return []string{namedInvocation: "named", inlineInvocation: "inline", referenceInvocation: "reference"}
}

func invocationForm(c *call) string {
	forms := AdmittedInvocationForms()
	if c.inline != nil {
		return forms[inlineInvocation]
	}
	if c.ref != nil {
		return forms[referenceInvocation]
	}
	return forms[namedInvocation]
}
