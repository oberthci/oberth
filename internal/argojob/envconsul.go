package argojob

// Admission for the legacy envconsul credential chain.
//
// The declared-secret-paths annotation is the auditable statement of what a
// pipeline reads, and `oberth secretstore exec` is admission-checked against
// it flag by flag. envconsul is different: WHAT it fetches lives in
// repository-authored configuration files (`secret {}` stanzas) and -secret
// command-line flags, not in the annotation. Before this gate existed, a
// branch author could declare one innocuous path in the annotation and add a
// `secret {}` stanza fetching any path the pod's Vault policy allowed — the
// annotation gated only the declaration while the fetch obeyed the files
// (issue #200, second manifestation).
//
// This gate re-joins the two: every secret path envconsul would fetch must
// appear in the same declared annotation the approval table authorized. The
// configuration is read out of the immutable run workspace — the exact bytes
// the seeder copies into the run's read-only source claim — so the admitted
// text and the executed text cannot diverge, and a config file outside that
// checkout (writable /tmp, another volume) is refused outright because
// nothing could vouch for its content.
//
// The Vault policy bound to the tier's ServiceAccount remains the
// authoritative boundary; this check enforces declared-intent honesty on top
// of it, the same defense-in-depth layering the approval table provides.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	hcl "github.com/hashicorp/hcl"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// maxEnvconsulValueDepth bounds the walk over decoded configuration values.
// envconsul's real config nests two levels; anything deeper is not a
// configuration envconsul would use, and an unbounded walk would let a pushed
// file choose the server's recursion depth.
const maxEnvconsulValueDepth = 32

// admitEnvconsulSecretPaths walks every template — inline ones included, since
// the credential mount injection reaches them too — and, for any whose main
// container or script command is envconsul, verifies that every secret path
// its invocation could fetch is covered by the workflow's declared annotation.
func admitEnvconsulSecretPaths(workflow *wfv1.Workflow, declaredPaths []string, sourceDir string) error {
	allowed := allowedStoreSpellings(declaredPaths)
	var problems []error
	walkTemplates(workflow, func(template *wfv1.Template) {
		if !templateUsesEnvconsul(template) {
			return
		}
		command, args := templateInvocation(template)
		configs := extractFlagValues(command, args, "config")
		secrets := extractFlagValues(command, args, "secret")
		if len(configs) == 0 && len(secrets) == 0 {
			// envconsul with no secret configuration fetches nothing; the
			// child simply runs. Nothing to check.
			return
		}
		if len(declaredPaths) == 0 {
			problems = append(problems, fmt.Errorf(
				"argojob: template %q invokes envconsul with credential configuration "+
					"but the workflow declares no %s annotation",
				template.Name, argoworkflow.SecretPathsAnnotation))
			return
		}
		for _, secret := range secrets {
			if _, ok := allowed[secret]; !ok {
				problems = append(problems, fmt.Errorf(
					"argojob: template %q passes -secret %q, which is not covered by the workflow's %s annotation",
					template.Name, secret, argoworkflow.SecretPathsAnnotation))
			}
		}
		for _, config := range configs {
			problems = append(problems, admitEnvconsulConfigFile(template.Name, config, allowed, sourceDir)...)
		}
	})
	return errors.Join(problems...)
}

// admitEnvconsulConfigFile checks one -config argument: it must name a
// regular file inside the run's immutable source checkout, it must parse as
// envconsul configuration, and every secret path it fetches must be covered
// by the declared annotation. Every failure mode refuses the submission —
// a configuration the server cannot read and understand is a configuration
// it cannot vouch for.
func admitEnvconsulConfigFile(templateName, configArg string, allowed map[string]string, sourceDir string) []error {
	relative, err := sourceRelativePath(configArg)
	if err != nil {
		return []error{fmt.Errorf("argojob: template %q -config %q: %w", templateName, configArg, err)}
	}
	if strings.TrimSpace(sourceDir) == "" {
		return []error{fmt.Errorf(
			"argojob: template %q -config %q cannot be admission-checked: no run workspace is available to read it from",
			templateName, configArg)}
	}
	content, err := readBoundedWorkspaceFile(sourceDir, relative)
	if err != nil {
		return []error{fmt.Errorf("argojob: template %q -config %q: %w", templateName, configArg, err)}
	}
	fetched, err := envconsulSecretPaths(content)
	if err != nil {
		return []error{fmt.Errorf("argojob: template %q -config %q is not admissible envconsul configuration: %w",
			templateName, configArg, err)}
	}
	var problems []error
	for _, fetchedPath := range fetched {
		if _, ok := allowed[fetchedPath]; !ok {
			problems = append(problems, fmt.Errorf(
				"argojob: template %q -config %q fetches %q, which is not covered by the workflow's %s annotation",
				templateName, configArg, fetchedPath, argoworkflow.SecretPathsAnnotation))
		}
	}
	return problems
}

// sourceRelativePath maps a -config argument onto a clean path relative to
// the source checkout, refusing anything that resolves outside it. Pipeline
// containers run with workingDir /work/src, so a relative path and the
// /work/src-anchored absolute spelling name the same file; every other
// absolute path — /tmp most importantly, which is a writable emptyDir an
// earlier container in the same pod could have filled — is refused.
func sourceRelativePath(configArg string) (string, error) {
	trimmed := strings.TrimSpace(configArg)
	if trimmed == "" {
		return "", errors.New("the configuration path is empty")
	}
	if strings.HasPrefix(trimmed, "/") {
		rest, ok := strings.CutPrefix(trimmed, SourceMountPath+"/")
		if !ok {
			return "", fmt.Errorf("the configuration must live inside the immutable source checkout (%s); "+
				"a file anywhere else cannot be admission-checked", SourceMountPath)
		}
		trimmed = rest
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("the configuration path escapes the source checkout")
	}
	return cleaned, nil
}

// readBoundedWorkspaceFile reads one bounded regular file from the immutable
// run workspace without escaping its root — the same constraints the app
// layer applies to the pipeline document itself.
func readBoundedWorkspaceFile(sourceDir, relative string) ([]byte, error) {
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("open the immutable run workspace: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(relative)
	if err != nil {
		return nil, fmt.Errorf("read it from the run workspace: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat it in the run workspace: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > periapsis.MaxSourceBytes {
		return nil, errors.New("it must be one bounded regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, periapsis.MaxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read it from the run workspace: %w", err)
	}
	if len(content) > periapsis.MaxSourceBytes {
		return nil, errors.New("it exceeds the source-size limit")
	}
	return content, nil
}

// envconsulSecretPaths parses one configuration file with the same HCL
// grammar envconsul itself uses (hashicorp/hcl v1 — HCL and its JSON form
// both) and returns every string assigned to a `path` key anywhere in it.
//
// Collecting every `path` — not only those under a `secret {}` stanza — is
// deliberate over-collection: a stanza this walk misinterprets can only make
// the check stricter, never open a fetch the annotation does not cover. A
// file the parser cannot decode is refused by the caller for the same
// reason.
func envconsulSecretPaths(content []byte) ([]string, error) {
	var decoded map[string]any
	if err := hcl.Unmarshal(content, &decoded); err != nil {
		return nil, err
	}
	var paths []string
	if err := collectPathValues(decoded, &paths, 0); err != nil {
		return nil, err
	}
	return paths, nil
}

func collectPathValues(value any, paths *[]string, depth int) error {
	if depth > maxEnvconsulValueDepth {
		return errors.New("the configuration nests deeper than envconsul configuration ever does")
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "path" {
				if err := collectPathStrings(nested, paths, depth+1); err != nil {
					return err
				}
				continue
			}
			if err := collectPathValues(nested, paths, depth+1); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, nested := range typed {
			if err := collectPathValues(nested, paths, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range typed {
			if err := collectPathValues(nested, paths, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectPathStrings accepts the value shapes HCL decoding can produce for a
// `path` assignment and refuses the ones that are not statically checkable
// strings — a non-string path is not a configuration this gate can vouch for.
func collectPathStrings(value any, paths *[]string, depth int) error {
	if depth > maxEnvconsulValueDepth {
		return errors.New("the configuration nests deeper than envconsul configuration ever does")
	}
	switch typed := value.(type) {
	case string:
		*paths = append(*paths, typed)
	case []any:
		for _, nested := range typed {
			if err := collectPathStrings(nested, paths, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("a `path` value must be a string, got %T", value)
	}
	return nil
}

// allowedStoreSpellings maps every spelling of a declared path that
// envconsul's Vault client would resolve to the same read back onto the
// declaration. Upstream-scoped declarations (oberth/upstream/<org>/...) are
// virtual: the store read happens at the KV v2 API path
// (<mount>/data/upstream/...), and envconsul accepts either spelling because
// its client rewrites logical KV v2 paths onto the data endpoint. System
// declarations are already API paths and gain their logical spelling for the
// same reason.
func allowedStoreSpellings(declaredPaths []string) map[string]string {
	upstreamMount := strings.SplitN(periapsis.UpstreamSecretPathPrefix, "/", 2)[0]
	allowed := make(map[string]string, len(declaredPaths)*2)
	for _, declared := range declaredPaths {
		allowed[declared] = declared
		if scoped, upstreamScoped, err := periapsis.ParseUpstreamSecretStorePath(declared); err == nil && upstreamScoped {
			allowed[scoped.FetchPath(upstreamMount)] = declared
			continue
		}
		segments := strings.SplitN(declared, "/", 3)
		if len(segments) == 3 && segments[1] == "data" {
			allowed[segments[0]+"/"+segments[2]] = declared
		}
	}
	return allowed
}

// templateInvocation returns the main container or script command and args —
// the same invocation templateUsesEnvconsul and the credential mount injection
// key off.
func templateInvocation(template *wfv1.Template) (command, args []string) {
	if template.Container != nil {
		return template.Container.Command, template.Container.Args
	}
	if template.Script != nil {
		return template.Script.Command, template.Script.Args
	}
	return nil, nil
}

// extractFlagValues collects every value of one envconsul flag across the
// full invocation, in all four spellings envconsul accepts (-name value,
// --name value, -name=value, --name=value).
//
// The scan deliberately does not stop at the child-command boundary: locating
// it would mean reimplementing envconsul's own flag table, and a mistake
// there in the permissive direction is a bypass. Scanning past the boundary
// can only over-collect, and over-collection fails closed.
func extractFlagValues(command, args []string, name string) []string {
	all := make([]string, 0, len(command)+len(args))
	all = append(all, command...)
	all = append(all, args...)
	var values []string
	for index := 0; index < len(all); index++ {
		argument := all[index]
		if argument == "-"+name || argument == "--"+name {
			if index+1 < len(all) {
				index++
				values = append(values, all[index])
			}
			continue
		}
		if rest, ok := strings.CutPrefix(argument, "-"+name+"="); ok {
			values = append(values, rest)
			continue
		}
		if rest, ok := strings.CutPrefix(argument, "--"+name+"="); ok {
			values = append(values, rest)
		}
	}
	return values
}

// walkTemplates visits every template of the workflow, inline templates
// included, with the same depth bound every other identity-bearing walk uses.
func walkTemplates(workflow *wfv1.Workflow, visit func(*wfv1.Template)) {
	for index := range workflow.Spec.Templates {
		walkTemplate(&workflow.Spec.Templates[index], visit, 0)
	}
}

func walkTemplate(template *wfv1.Template, visit func(*wfv1.Template), depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	visit(template)
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			walkTemplate(template.Steps[group].Steps[step].Inline, visit, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			walkTemplate(template.DAG.Tasks[task].Inline, visit, depth+1)
		}
	}
}
