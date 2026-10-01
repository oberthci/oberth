package argojob

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// trivyGuard refuses an Argo Workflow whose DAG contains two Trivy leaves
// that are UNORDERED (no DAG path between them) and whose --cache-dir
// resolves to the SAME shared volume. Without an ordering edge, both leaves
// can start concurrently and corrupt each other's bbolt DB via mmap.
//
// Issue #655: the s3-proxy red shape (7bf51648) had two unordered Trivy
// leaves on the same "work" VCT subPath "trivy-cache". The second leaf's
// download rewrote the bbolt file under the first leaf's mmap, causing a
// SIGSEGV in bbolt.FastCheck.
//
// The predicate is "two unordered Trivy leaves whose cache path resolves to
// the same shared volume": a VCT mount, or the server-injected workspace
// mount when workspace-mounts != none. Matching the --cache-dir string alone
// would false-positive on a CLI repository whose Trivy leaves use
// container-private /tmp (workspace-mounts: none).
func trivyGuard(workflow *wfv1.Workflow) error {
	entrypoint := strings.TrimSpace(workflow.Spec.Entrypoint)
	if entrypoint == "" {
		return nil
	}
	templates := indexTemplates(workflow)
	root := templates[entrypoint]
	if root == nil || root.DAG == nil {
		// Steps templates are sequential within each group; the race requires
		// concurrent execution, which is only possible in a DAG.
		return nil
	}
	hasWorkVCT := hasWorkspaceVCT(workflow)
	type trivyTask struct {
		name     string
		cacheDir string
	}
	var trivyTasks []trivyTask
	for _, task := range root.DAG.Tasks {
		resolved := resolveTemplate(task, templates)
		if resolved == nil {
			continue
		}
		if !templateUsesTrivyBinary(resolved) {
			continue
		}
		cacheDir := extractTrivyCacheDir(resolved)
		if cacheDir == "" {
			continue
		}
		if !trivyCacheIsShared(resolved, cacheDir, hasWorkVCT, workflow) {
			continue
		}
		trivyTasks = append(trivyTasks, trivyTask{name: task.Name, cacheDir: cacheDir})
	}
	if len(trivyTasks) < 2 {
		return nil
	}
	reachable := dagReachability(root.DAG)
	var problems []error
	for i := 0; i < len(trivyTasks); i++ {
		for j := i + 1; j < len(trivyTasks); j++ {
			a, b := trivyTasks[i], trivyTasks[j]
			if a.cacheDir != b.cacheDir {
				continue
			}
			if reachable[a.name][b.name] || reachable[b.name][a.name] {
				continue
			}
			problems = append(problems, fmt.Errorf(
				"argojob: DAG tasks %q and %q both run Trivy with --cache-dir %q on a shared volume "+
					"but have no ordering edge between them; without an edge, concurrent Trivy DB downloads "+
					"corrupt each other's bbolt mmap (issue #655). Add a depends edge to serialize them",
				a.name, b.name, a.cacheDir))
		}
	}
	return errors.Join(problems...)
}

// indexTemplates builds a name-to-template map for by-name resolution.
func indexTemplates(workflow *wfv1.Workflow) map[string]*wfv1.Template {
	templates := make(map[string]*wfv1.Template, len(workflow.Spec.Templates))
	for i := range workflow.Spec.Templates {
		t := &workflow.Spec.Templates[i]
		if _, exists := templates[t.Name]; !exists {
			templates[t.Name] = t
		}
	}
	return templates
}

// resolveTemplate returns the template a DAG task invokes: its inline
// template if present, otherwise the by-name template.
func resolveTemplate(task wfv1.DAGTask, templates map[string]*wfv1.Template) *wfv1.Template {
	if task.Inline != nil {
		return task.Inline
	}
	return templates[task.Template]
}

// templateUsesTrivyBinary reports whether a template's main container command
// invokes the Trivy binary.
func templateUsesTrivyBinary(template *wfv1.Template) bool {
	isTrivy := func(command []string, args []string) bool {
		all := append(command, args...)
		for _, arg := range all {
			if arg == "trivy" || strings.HasSuffix(arg, "/trivy") {
				return true
			}
		}
		return false
	}
	if template.Container != nil {
		return isTrivy(template.Container.Command, template.Container.Args)
	}
	if template.Script != nil {
		return isTrivy(template.Script.Command, template.Script.Args)
	}
	return false
}

// extractTrivyCacheDir returns the --cache-dir value from a Trivy invocation.
// Returns empty string if no --cache-dir is specified.
func extractTrivyCacheDir(template *wfv1.Template) string {
	extract := func(command, args []string) string {
		all := append(command, args...)
		for i := 0; i < len(all); i++ {
			if rest, found := strings.CutPrefix(all[i], "--cache-dir="); found {
				return rest
			}
			if all[i] == "--cache-dir" && i+1 < len(all) {
				return all[i+1]
			}
		}
		return ""
	}
	if template.Container != nil {
		return extract(template.Container.Command, template.Container.Args)
	}
	if template.Script != nil {
		return extract(template.Script.Command, template.Script.Args)
	}
	return ""
}

// trivyCacheIsShared reports whether the given cache directory path resolves
// to a shared volume that is visible to multiple steps in the same workflow.
//
// A cache dir is shared when:
//   - The workflow has a "work" VCT AND the template does not opt out of
//     workspace mounts (workspace-mounts != none) AND the cache dir is the
//     server-injected trivy-cache mount path.
//   - The template has a repo-declared volume mount from a VCT at the cache
//     dir path.
//
// A cache dir is private when workspace-mounts is "none" and the path lands
// on the per-Pod emptyDir (/tmp), which is what repository-authored Trivy leaves
// use.
func trivyCacheIsShared(template *wfv1.Template, cacheDir string, hasWorkVCT bool, workflow *wfv1.Workflow) bool {
	// Check if workspace mounts are disabled for this template.
	if workspaceMountMode(template) == "none" {
		// Even with workspace mounts disabled, check for repo-declared shared
		// volume mounts at the same path.
		return templateHasSharedVolumeAt(template, cacheDir, workflow)
	}
	// When workspace mounts are not disabled and the workflow has a work VCT,
	// the server injects a mount from the work VCT at WorkspaceTrivyCacheMountPath.
	if hasWorkVCT && cacheDir == WorkspaceTrivyCacheMountPath {
		return true
	}
	// Check repo-declared mounts.
	return templateHasSharedVolumeAt(template, cacheDir, workflow)
}

// templateHasSharedVolumeAt reports whether the template has a repo-declared
// volume mount at the given path that is backed by a VCT (shared across steps).
func templateHasSharedVolumeAt(template *wfv1.Template, mountPath string, workflow *wfv1.Workflow) bool {
	vctNames := make(map[string]bool, len(workflow.Spec.VolumeClaimTemplates))
	for _, claim := range workflow.Spec.VolumeClaimTemplates {
		vctNames[claim.Name] = true
	}
	shared := false
	templateContainers(template, func(c *corev1.Container) {
		for _, m := range c.VolumeMounts {
			if m.MountPath == mountPath && vctNames[m.Name] {
				shared = true
			}
		}
	})
	return shared
}

// dagReachability builds a transitive closure of the DAG's task dependencies.
// reachable[a][b] == true means there is a path from a to b.
func dagReachability(dag *wfv1.DAGTemplate) map[string]map[string]bool {
	if dag == nil {
		return nil
	}
	// Build adjacency list from depends/dependencies.
	children := make(map[string][]string, len(dag.Tasks))
	for _, task := range dag.Tasks {
		deps := parseDependencies(task)
		for _, dep := range deps {
			children[dep] = append(children[dep], task.Name)
		}
	}
	// Compute transitive closure via BFS from each node.
	reachable := make(map[string]map[string]bool, len(dag.Tasks))
	for _, task := range dag.Tasks {
		visited := make(map[string]bool)
		queue := children[task.Name]
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if visited[current] {
				continue
			}
			visited[current] = true
			queue = append(queue, children[current]...)
		}
		if len(visited) > 0 {
			reachable[task.Name] = visited
		}
	}
	return reachable
}

// parseDependencies extracts the task names a DAG task depends on, supporting
// both the enhanced depends syntax (string with && / ||) and the legacy
// dependencies list.
func parseDependencies(task wfv1.DAGTask) []string {
	if depends := strings.TrimSpace(task.Depends); depends != "" {
		return parseEnhancedDepends(depends)
	}
	return task.Dependencies
}

// parseEnhancedDepends extracts task names from Argo's enhanced depends syntax.
// The syntax uses task names with optional .Succeeded/.Failed suffixes,
// combined with && and ||. We extract just the base task names.
func parseEnhancedDepends(depends string) []string {
	// Split on && and || operators, then extract task names.
	var names []string
	seen := make(map[string]bool)
	for _, token := range strings.FieldsFunc(depends, func(r rune) bool {
		return r == '&' || r == '|' || r == '(' || r == ')' || r == ' '
	}) {
		// Remove status suffixes like .Succeeded, .Failed, .Daemoned, .Errored
		name, _, _ := strings.Cut(token, ".")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// Reference the argoworkflow import to avoid an unused import warning.
var _ = argoworkflow.WorkspaceMountsAnnotation
