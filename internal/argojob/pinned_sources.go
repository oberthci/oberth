package argojob

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
	corev1 "k8s.io/api/core/v1"
)

type PinnedSourceTree struct {
	Source    pipeline.PinnedSource
	Directory string
}

func validatePinnedSourceDelivery(workflow *wfv1.Workflow, request Request) error {
	declared, err := argoworkflow.PinnedSources(workflow)
	if err != nil {
		return err
	}
	actual := make([]pipeline.PinnedSource, 0, len(request.PinnedSources))
	for _, tree := range request.PinnedSources {
		if tree.Directory == "" {
			return errors.New("argojob: pinned source has no verified tree")
		}
		actual = append(actual, tree.Source)
	}
	if !slices.Equal(declared, actual) {
		return errors.New("argojob: pinned sources require exact server-authorized tree bindings")
	}
	// Repository-authored mounts must not shadow any portion of delivered
	// sources. The normal server mount pass handles all container shapes.
	if workflow.Spec.TemplateDefaults != nil {
		for _, source := range actual {
			var conflict bool
			walkTemplateContainers(workflow.Spec.TemplateDefaults, func(m string) {
				a := path.Clean(m)
				b := source.Path
				if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
					conflict = true
				}
			}, 0)
			if conflict {
				return errors.New("argojob: template defaults shadow a pinned source path")
			}
		}
	}
	for _, template := range workflow.Spec.Templates {
		var conflict bool
		walkTemplateContainers(&template, func(mountPath string) {
			for _, source := range actual {
				a, b := path.Clean(mountPath), source.Path
				if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
					conflict = true
				}
			}
		}, 0)
		if conflict {
			return fmt.Errorf("argojob: template %s shadows a pinned source path", template.Name)
		}
	}
	return nil
}

func walkTemplateContainers(template *wfv1.Template, visit func(string), depth int) {
	if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
		return
	}
	templateContainers(template, func(c *corev1.Container) {
		for _, mount := range c.VolumeMounts {
			visit(mount.MountPath)
		}
	})
	if template.ContainerSet != nil {
		for _, m := range template.ContainerSet.VolumeMounts {
			visit(m.MountPath)
		}
	}
	for _, group := range template.Steps {
		for _, step := range group.Steps {
			walkTemplateContainers(step.Inline, visit, depth+1)
		}
	}
	if template.DAG != nil {
		for _, task := range template.DAG.Tasks {
			walkTemplateContainers(task.Inline, visit, depth+1)
		}
	}
}

// Live retry/adoption verifies the complete source footprint independently of
// the historical identity normalization that omits source-volume mounts.
func verifyDeliveredPinnedSources(workflow *wfv1.Workflow, expected []pipeline.PinnedSource) error {
	declared, err := argoworkflow.PinnedSources(workflow)
	if err != nil || !slices.Equal(declared, expected) {
		return errors.New("argojob: live pinned source binding mismatch")
	}
	if len(expected) == 0 {
		return nil
	}
	claimOK := false
	for _, v := range workflow.Spec.Volumes {
		if v.Name == SourceVolumeName && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == sourceClaimName(workflow.Name) {
			claimOK = true
		}
	}
	if !claimOK {
		return errors.New("argojob: live pinned source claim mismatch")
	}
	var bad bool
	// Defaults and container-set common mounts are not server source mounts.
	// They must not introduce an alias or shadow outside member containers.
	checkExtra := func(m corev1.VolumeMount) {
		for _, source := range expected {
			a := path.Clean(m.MountPath)
			if a == "/" || a == source.Path || strings.HasPrefix(a, source.Path+"/") || strings.HasPrefix(source.Path, a+"/") || (m.Name == SourceVolumeName && !m.ReadOnly) {
				bad = true
			}
		}
	}
	if workflow.Spec.TemplateDefaults != nil {
		templateContainers(workflow.Spec.TemplateDefaults, func(c *corev1.Container) {
			for _, m := range c.VolumeMounts {
				checkExtra(m)
			}
		})
		if workflow.Spec.TemplateDefaults.ContainerSet != nil {
			for _, m := range workflow.Spec.TemplateDefaults.ContainerSet.VolumeMounts {
				checkExtra(m)
			}
		}
	}
	var inspect func(*wfv1.Template, int)
	inspect = func(template *wfv1.Template, depth int) {
		if template == nil || depth > argoworkflow.MaxIdentityWalkDepth {
			return
		}
		if template.ContainerSet != nil {
			for _, m := range template.ContainerSet.VolumeMounts {
				checkExtra(m)
			}
		}
		templateContainers(template, func(c *corev1.Container) {
			for i, source := range expected {
				count := 0
				for _, m := range c.VolumeMounts {
					if m.MountPath == source.Path {
						count++
						if m.Name != SourceVolumeName || !m.ReadOnly || m.SubPath != fmt.Sprintf("pinned-sources/%d", i) || m.SubPathExpr != "" {
							bad = true
						}
					}
					if m.MountPath != source.Path && (path.Clean(m.MountPath) == "/" || strings.HasPrefix(path.Clean(m.MountPath), source.Path+"/") || strings.HasPrefix(source.Path, path.Clean(m.MountPath)+"/")) {
						bad = true
					}
					if m.Name == SourceVolumeName && m.MountPath != source.Path && (m.SubPathExpr != "" || m.SubPath == "" || m.SubPath == "." || path.Clean(m.SubPath) != m.SubPath || m.SubPath == "pinned-sources" || strings.HasPrefix(m.SubPath, "pinned-sources/")) && !m.ReadOnly {
						bad = true
					}
				}
				if count != 1 {
					bad = true
				}
			}
		})
		for _, g := range template.Steps {
			for _, s := range g.Steps {
				inspect(s.Inline, depth+1)
			}
		}
		if template.DAG != nil {
			for _, task := range template.DAG.Tasks {
				inspect(task.Inline, depth+1)
			}
		}
	}
	for i := range workflow.Spec.Templates {
		inspect(&workflow.Spec.Templates[i], 0)
	}
	if bad {
		return errors.New("argojob: live pinned source mount is missing, writable or shadowed")
	}
	return nil
}
