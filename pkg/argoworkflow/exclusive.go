package argoworkflow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// ExclusiveAnnotation is a step-level (template metadata) declaration that the
// step uses a server-wide exclusive resource class (#1499). Oberth admits at
// most the administrator's configured number of runs containing such a step
// at once, across every repository on the server. Excess runs wait in
// Oberth's admission queue, not as Kubernetes Pending Pods. The class must be
// on the administrator's allowlist; the feature is off while it is empty.
// The annotation adds no node reach: it only selects a server-owned queue.
const ExclusiveAnnotation = "oberth.ci/exclusive"

// MaxExclusiveClassLength bounds one class name; MaxExclusiveClassesPerRun
// bounds how many distinct classes one pipeline can declare.
const (
	MaxExclusiveClassLength   = 40
	MaxExclusiveClassesPerRun = 4
	MaxExclusiveClassLimit    = 64
	MaxExclusiveClasses       = 32
)

// ValidExclusiveClass reports whether name is 1..40 lowercase letters, digits
// and inner hyphens, starting and ending with a letter or digit.
func ValidExclusiveClass(name string) bool {
	if name == "" || len(name) > MaxExclusiveClassLength || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

// DeclaredExclusiveClasses returns the sorted distinct classes the document's
// templates declare, including template defaults and inline templates. A run
// is a member of every class one of its steps declares.
func DeclaredExclusiveClasses(workflow *wfv1.Workflow) ([]string, error) {
	if workflow == nil {
		return nil, fmt.Errorf("argoworkflow: no Workflow to read")
	}
	seen := map[string]bool{}
	var problem error
	var walk func(*wfv1.Template, int)
	walk = func(template *wfv1.Template, depth int) {
		if template == nil || problem != nil {
			return
		}
		if depth > MaxIdentityWalkDepth {
			problem = fmt.Errorf("argoworkflow: inline templates nest deeper than %d levels", MaxIdentityWalkDepth)
			return
		}
		if raw, ok := template.Metadata.Annotations[ExclusiveAnnotation]; ok {
			class := strings.TrimSpace(raw)
			if !ValidExclusiveClass(class) {
				problem = fmt.Errorf("argoworkflow: annotation %s declares invalid exclusive class %q; a class is 1 to %d lowercase letters, digits and inner hyphens", ExclusiveAnnotation, raw, MaxExclusiveClassLength)
				return
			}
			seen[class] = true
		}
		for group := range template.Steps {
			for step := range template.Steps[group].Steps {
				walk(template.Steps[group].Steps[step].Inline, depth+1)
			}
		}
		if template.DAG != nil {
			for task := range template.DAG.Tasks {
				walk(template.DAG.Tasks[task].Inline, depth+1)
			}
		}
	}
	for index := range workflow.Spec.Templates {
		walk(&workflow.Spec.Templates[index], 0)
	}
	walk(workflow.Spec.TemplateDefaults, 0)
	if problem != nil {
		return nil, problem
	}
	if len(seen) > MaxExclusiveClassesPerRun {
		return nil, fmt.Errorf("argoworkflow: annotation %s declares %d exclusive classes; at most %d are allowed per pipeline", ExclusiveAnnotation, len(seen), MaxExclusiveClassesPerRun)
	}
	classes := make([]string, 0, len(seen))
	for class := range seen {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	return classes, nil
}

// ParseExclusiveClasses reads the administrator allowlist: comma-separated
// `class` or `class=limit` entries. A missing limit is one. Empty disables
// the feature: every declaration is then refused.
func ParseExclusiveClasses(value string) (map[string]int, error) {
	classes := map[string]int{}
	if strings.TrimSpace(value) == "" {
		return classes, nil
	}
	for _, entry := range strings.Split(value, ",") {
		name, rawLimit, hasLimit := strings.Cut(strings.TrimSpace(entry), "=")
		limit := 1
		if hasLimit {
			parsed, err := strconv.Atoi(rawLimit)
			if err != nil || parsed < 1 || parsed > MaxExclusiveClassLimit || strconv.Itoa(parsed) != rawLimit {
				return nil, fmt.Errorf("exclusive class %q: limit must be an integer from 1 to %d", name, MaxExclusiveClassLimit)
			}
			limit = parsed
		}
		if !ValidExclusiveClass(name) {
			return nil, fmt.Errorf("exclusive class %q: a class is 1 to %d lowercase letters, digits and inner hyphens", name, MaxExclusiveClassLength)
		}
		if _, duplicate := classes[name]; duplicate {
			return nil, fmt.Errorf("exclusive class %q is listed twice", name)
		}
		classes[name] = limit
	}
	if len(classes) > MaxExclusiveClasses {
		return nil, fmt.Errorf("at most %d exclusive classes can be configured", MaxExclusiveClasses)
	}
	return classes, nil
}
