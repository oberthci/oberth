package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

const (
	MaxFragments        = 16
	maxTemplateNameChar = 63
	fragmentNamePrefix  = "frag-"
)

var inlinedNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][-a-zA-Z0-9]*$`)

type FragmentKey struct {
	Repo    string `json:"repo"`
	Version string `json:"version"`
}

func (key FragmentKey) String() string { return key.Repo + "@" + key.Version }

type Fragment struct {
	Key    FragmentKey `json:"key"`
	SHA    string      `json:"sha"`
	Digest string      `json:"digest"`
	Source []byte      `json:"-"`
}

type Lock []Fragment

func ParseFragmentRef(ref string) (FragmentKey, error) {
	if ref == "" {
		return FragmentKey{}, errors.New("argoworkflow: empty fragment reference")
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return FragmentKey{}, fmt.Errorf("argoworkflow: fragment reference %q contains whitespace", ref)
	}
	repo, version, found := strings.Cut(ref, "@")
	if !found {
		return FragmentKey{}, fmt.Errorf("argoworkflow: fragment reference %q has no @version; a fragment must be pinned", ref)
	}
	if strings.Contains(version, "@") {
		return FragmentKey{}, fmt.Errorf("argoworkflow: fragment reference %q has more than one @", ref)
	}
	if repo == "" || version == "" {
		return FragmentKey{}, fmt.Errorf("argoworkflow: fragment reference %q must be repository@version", ref)
	}
	if err := validateFragmentRepo(repo); err != nil {
		return FragmentKey{}, err
	}
	if strings.Contains(version, "/") || version == "." || version == ".." {
		return FragmentKey{}, fmt.Errorf("argoworkflow: fragment version %q is not a tag name", version)
	}
	return FragmentKey{Repo: repo, Version: version}, nil
}

func validateFragmentRepo(repo string) error {
	if strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return fmt.Errorf("argoworkflow: fragment repository %q must be a relative path", repo)
	}
	for _, segment := range strings.Split(repo, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("argoworkflow: fragment repository %q must not contain %q", repo, segment)
		}
	}
	return nil
}

type templateRefSite struct {
	ref   *wfv1.TemplateRef
	where string
	bind  func(name string)
}

func FragmentRefs(workflow *wfv1.Workflow) ([]FragmentKey, error) {
	if workflow == nil {
		return nil, nil
	}
	var keys []FragmentKey
	seen := map[FragmentKey]bool{}
	for _, site := range templateRefSites(workflow) {
		key, err := ParseFragmentRef(site.ref.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", site.where, err)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if len(keys) > MaxFragments {
		return nil, fmt.Errorf("argoworkflow: document references %d fragments, the limit is %d", len(keys), MaxFragments)
	}
	return keys, nil
}

func templateRefSites(workflow *wfv1.Workflow) []templateRefSite {
	var sites []templateRefSite
	collectHookSites(workflow.Spec.Hooks, "spec.hooks", &sites)
	for index := range workflow.Spec.Templates {
		collectTemplateSites(&workflow.Spec.Templates[index],
			fmt.Sprintf("spec.templates[%q]", workflow.Spec.Templates[index].Name), &sites)
	}
	return sites
}

func collectTemplateSites(template *wfv1.Template, where string, sites *[]templateRefSite) {
	for group := range template.Steps {
		for index := range template.Steps[group].Steps {
			step := &template.Steps[group].Steps[index]
			location := fmt.Sprintf("%s.steps[%d][%d]", where, group, index)
			if step.TemplateRef != nil {
				*sites = append(*sites, templateRefSite{ref: step.TemplateRef, where: location, bind: func(name string) {
					step.TemplateRef = nil
					step.Template = name
				}})
			}
			collectHookSites(step.Hooks, location+".hooks", sites)
			if step.Inline != nil {
				collectTemplateSites(step.Inline, location+".inline", sites)
			}
		}
	}
	if template.DAG != nil {
		for index := range template.DAG.Tasks {
			task := &template.DAG.Tasks[index]
			location := fmt.Sprintf("%s.dag.tasks[%q]", where, task.Name)
			if task.TemplateRef != nil {
				*sites = append(*sites, templateRefSite{ref: task.TemplateRef, where: location, bind: func(name string) {
					task.TemplateRef = nil
					task.Template = name
				}})
			}
			collectHookSites(task.Hooks, location+".hooks", sites)
			if task.Inline != nil {
				collectTemplateSites(task.Inline, location+".inline", sites)
			}
		}
	}
}

func collectHookSites(hooks wfv1.LifecycleHooks, where string, sites *[]templateRefSite) {
	if len(hooks) == 0 {
		return
	}
	events := make([]string, 0, len(hooks))
	for event := range hooks {
		events = append(events, string(event))
	}
	sort.Strings(events)
	for _, name := range events {
		event := wfv1.LifecycleEvent(name)
		hook := hooks[event]
		if hook.TemplateRef == nil {
			continue
		}
		*sites = append(*sites, templateRefSite{
			ref:   hook.TemplateRef,
			where: fmt.Sprintf("%s[%q]", where, name),
			bind: func(bound string) {
				updated := hooks[event]
				updated.TemplateRef = nil
				updated.Template = bound
				hooks[event] = updated
			},
		})
	}
}

func Resolve(workflow *wfv1.Workflow, fragments map[FragmentKey]Fragment) (Lock, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to resolve")
	}
	if workflow.Spec.WorkflowTemplateRef != nil {
		return nil, errors.New("argoworkflow: spec.workflowTemplateRef names a whole spec, not a template, and cannot be resolved to a fragment; inline the templates")
	}
	sites := templateRefSites(workflow)
	if len(sites) == 0 {
		return nil, nil
	}
	keys, err := FragmentRefs(workflow)
	if err != nil {
		return nil, err
	}

	existing := map[string]bool{}
	for _, template := range workflow.Spec.Templates {
		existing[template.Name] = true
	}

	renames := map[FragmentKey]map[string]string{}
	var inlined []wfv1.Template
	lock := make(Lock, 0, len(keys))

	for _, key := range keys {
		fragment, ok := fragments[key]
		if !ok {
			return nil, fmt.Errorf("argoworkflow: fragment %s was not loaded", key)
		}
		templates, err := fragmentTemplates(key, fragment)
		if err != nil {
			return nil, err
		}
		mapping := map[string]string{}
		for _, template := range templates {
			name, err := inlinedName(key, template.Name)
			if err != nil {
				return nil, err
			}
			if existing[name] {
				return nil, fmt.Errorf("argoworkflow: fragment %s inlines template %q as %q, which the document already defines",
					key, template.Name, name)
			}
			existing[name] = true
			mapping[template.Name] = name
		}
		renames[key] = mapping
		for _, template := range templates {
			copied := *template.DeepCopy()
			copied.Name = mapping[template.Name]
			rebindLocalTemplates(&copied, mapping)
			inlined = append(inlined, copied)
		}
		digest := sha256.Sum256(fragment.Source)
		lock = append(lock, Fragment{
			Key:    key,
			SHA:    fragment.SHA,
			Digest: hex.EncodeToString(digest[:]),
		})
	}

	for _, site := range sites {
		key, err := ParseFragmentRef(site.ref.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", site.where, err)
		}
		bound, ok := renames[key][site.ref.Template]
		if !ok {
			return nil, fmt.Errorf("argoworkflow: %s references template %q, which fragment %s does not define",
				site.where, site.ref.Template, key)
		}
		site.bind(bound)
	}
	workflow.Spec.Templates = append(workflow.Spec.Templates, inlined...)
	return lock, nil
}

func fragmentTemplates(key FragmentKey, fragment Fragment) ([]wfv1.Template, error) {
	document, err := Decode(fragment.Source)
	if err != nil {
		return nil, fmt.Errorf("argoworkflow: fragment %s: %w", key, err)
	}
	if len(document.Spec.Templates) == 0 {
		return nil, fmt.Errorf("argoworkflow: fragment %s defines no templates", key)
	}
	if document.Spec.WorkflowTemplateRef != nil {
		return nil, fmt.Errorf("argoworkflow: fragment %s declares spec.workflowTemplateRef; fragments do not nest", key)
	}
	if nested := templateRefSites(document); len(nested) > 0 {
		return nil, fmt.Errorf("argoworkflow: fragment %s references another fragment at %s; fragments do not nest",
			key, nested[0].where)
	}
	return document.Spec.Templates, nil
}

func inlinedName(key FragmentKey, template string) (string, error) {
	digest := sha256.Sum256([]byte(key.String()))
	name := fragmentNamePrefix + hex.EncodeToString(digest[:4]) + "-" + template
	if len(name) > maxTemplateNameChar {
		return "", fmt.Errorf("argoworkflow: fragment %s template %q inlines to %q, longer than the %d character limit",
			key, template, name, maxTemplateNameChar)
	}
	if !inlinedNamePattern.MatchString(name) {
		return "", fmt.Errorf("argoworkflow: fragment %s template %q inlines to %q, which is not a valid template name",
			key, template, name)
	}
	return name, nil
}

func rebindLocalTemplates(template *wfv1.Template, mapping map[string]string) {
	for group := range template.Steps {
		for index := range template.Steps[group].Steps {
			step := &template.Steps[group].Steps[index]
			if bound, ok := mapping[step.Template]; ok {
				step.Template = bound
			}
			rebindHooks(step.Hooks, mapping)
			if step.Inline != nil {
				rebindLocalTemplates(step.Inline, mapping)
			}
		}
	}
	if template.DAG != nil {
		for index := range template.DAG.Tasks {
			task := &template.DAG.Tasks[index]
			if bound, ok := mapping[task.Template]; ok {
				task.Template = bound
			}
			rebindHooks(task.Hooks, mapping)
			if task.Inline != nil {
				rebindLocalTemplates(task.Inline, mapping)
			}
		}
	}
}

func rebindHooks(hooks wfv1.LifecycleHooks, mapping map[string]string) {
	for event, hook := range hooks {
		if bound, ok := mapping[hook.Template]; ok {
			hook.Template = bound
			hooks[event] = hook
		}
	}
}
