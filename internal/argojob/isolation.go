package argojob

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func tokenDisabled(t *wfv1.Template) bool {
	return t.AutomountServiceAccountToken != nil && !*t.AutomountServiceAccountToken
}

func workspaceMountMode(t *wfv1.Template) string {
	return t.Metadata.Annotations[argoworkflow.WorkspaceMountsAnnotation]
}

func workspaceEnvDisabled(t *wfv1.Template) bool {
	return t.Metadata.Annotations[argoworkflow.WorkspaceEnvAnnotation] == "none" || workspaceMountMode(t) == "none"
}

func templateContainers(t *wfv1.Template, visit func(*corev1.Container)) {
	if t.Container != nil {
		visit(t.Container)
	}
	if t.Script != nil {
		visit(&t.Script.Container)
	}
	if t.ContainerSet != nil {
		for i := range t.ContainerSet.Containers {
			visit(&t.ContainerSet.Containers[i].Container)
		}
	}
	for i := range t.InitContainers {
		visit(&t.InitContainers[i].Container)
	}
	for i := range t.Sidecars {
		visit(&t.Sidecars[i].Container)
	}
}

func templateMainContainer(t *wfv1.Template) *corev1.Container {
	if t.Container != nil {
		return t.Container
	}
	if t.Script != nil {
		return &t.Script.Container
	}
	if t.ContainerSet != nil {
		for i := range t.ContainerSet.Containers {
			if t.ContainerSet.Containers[i].Name == "main" {
				return &t.ContainerSet.Containers[i].Container
			}
		}
	}
	return nil
}

// Argo appends common mounts without deduplicating them. Materialize those
// declarations before applying server mount policy, retaining node-local paths.
func normalizeContainerSetMounts(wf *wfv1.Workflow) {
	walkTemplates(wf, func(t *wfv1.Template) {
		if t.ContainerSet == nil {
			return
		}
		for i := range t.ContainerSet.Containers {
			c := &t.ContainerSet.Containers[i].Container
			for _, common := range t.ContainerSet.VolumeMounts {
				found := false
				for j := range c.VolumeMounts {
					if c.VolumeMounts[j].MountPath == common.MountPath {
						found = true
						c.VolumeMounts[j].ReadOnly = c.VolumeMounts[j].ReadOnly || common.ReadOnly
					}
				}
				if !found {
					c.VolumeMounts = append(c.VolumeMounts, common)
				}
			}
		}
		t.ContainerSet.VolumeMounts = nil
	})
}

// Argo 4.0.8 discovers container-set volumes only through common mounts,
// ignoring node-local mounts. Bind only referenced volumes into the Pod patch.
// VCT names follow controller/operator.go:createPVCs and stay run-owned.
func containerSetPodVolumes(wf *wfv1.Workflow, t *wfv1.Template) ([]corev1.Volume, error) {
	var volumes []corev1.Volume
	known := map[string]corev1.Volume{}
	for _, claim := range wf.Spec.VolumeClaimTemplates {
		known[claim.Name] = corev1.Volume{Name: claim.Name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: wf.Name + "-" + claim.Name}}}
	}
	for _, v := range wf.Spec.Volumes {
		known[v.Name] = v
	}
	for _, v := range t.Volumes {
		known[v.Name] = v
	}
	seen := map[string]bool{}
	for _, c := range t.ContainerSet.Containers {
		for _, m := range c.VolumeMounts {
			if seen[m.Name] {
				continue
			}
			v, ok := known[m.Name]
			if !ok {
				return nil, fmt.Errorf("argojob: containerSet %q references unknown volume %q", t.Name, m.Name)
			}
			volumes = append(volumes, v)
			seen[m.Name] = true
		}
	}
	return volumes, nil
}

// Resolve authored mirroring before Argo appends main's mounts a second time.
// This runs before credential injection: generated token/secret mounts must
// never be copied into repository sidecars or init containers.
func resolveUserMountMirrors(t *wfv1.Template) {
	main := templateMainContainer(t)
	resolve := func(c *wfv1.UserContainer) {
		if c.MirrorVolumeMounts == nil || !*c.MirrorVolumeMounts {
			return
		}
		c.MirrorVolumeMounts = nil
		if main == nil {
			return
		}
		paths := make(map[string]bool, len(c.VolumeMounts))
		for _, m := range c.VolumeMounts {
			paths[m.MountPath] = true
		}
		for _, m := range main.VolumeMounts {
			if !paths[m.MountPath] {
				c.VolumeMounts = append(c.VolumeMounts, m)
				paths[m.MountPath] = true
			}
		}
	}
	for i := range t.InitContainers {
		resolve(&t.InitContainers[i])
	}
	for i := range t.Sidecars {
		resolve(&t.Sidecars[i])
	}
}

func sharedWorkspaceMount(m corev1.VolumeMount) bool {
	return m.Name == WorkspaceVolumeName || m.Name == CacheVolumeName || m.Name == ArtifactsVolumeName
}

// A readonly request protects the underlying directory, not just one path in
// one container. Parent mounts and dynamic subpaths cannot restore write access.
func mountOverlaps(a, b corev1.VolumeMount) bool {
	if a.Name != b.Name {
		return false
	}
	if a.SubPathExpr != "" || b.SubPathExpr != "" {
		return true
	}
	x, y := path.Clean("/"+a.SubPath), path.Clean("/"+b.SubPath)
	return x == y || x == "/" || y == "/" || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/")
}

func readonlyMounts(t *wfv1.Template) []corev1.VolumeMount {
	var protected []corev1.VolumeMount
	if t.ContainerSet != nil {
		for _, m := range t.ContainerSet.VolumeMounts {
			if m.ReadOnly {
				protected = append(protected, m)
			}
		}
	}
	templateContainers(t, func(c *corev1.Container) {
		for _, m := range c.VolumeMounts {
			// These names are stripped from authored mounts. Do not turn a
			// generated source/token mount into a new isolation request on
			// otherwise unchanged workflows (including recovered executions).
			if m.Name == SourceVolumeName || m.Name == ReleaseTokenVolumeName || m.Name == SecretsVolumeName {
				continue
			}
			if m.ReadOnly {
				protected = append(protected, m)
			}
		}
	})
	return protected
}

func preserveWorkspaceReadOnly(t *wfv1.Template, protected []corev1.VolumeMount) {
	templateContainers(t, func(c *corev1.Container) {
		for i := range c.VolumeMounts {
			m := &c.VolumeMounts[i]
			if workspaceMountMode(t) == "readonly" && sharedWorkspaceMount(*m) {
				m.ReadOnly = true
			}
			for _, p := range protected {
				if mountOverlaps(*m, p) {
					m.ReadOnly = true
				}
			}
		}
	})
}

func validateTemplateIsolation(wf *wfv1.Workflow) error {
	var problems []error
	walkTemplates(wf, func(t *wfv1.Template) {
		if tokenDisabled(t) && templateUsesCredentialChain(t) {
			problems = append(problems, fmt.Errorf("argojob: template %q disables its service-account token but invokes a credential chain", t.Name))
		}
		if tokenDisabled(t) && wf.Spec.TemplateDefaults != nil && templateUsesCredentialChain(wf.Spec.TemplateDefaults) {
			problems = append(problems, fmt.Errorf("argojob: tokenless template %q cannot inherit a credential chain from templateDefaults", t.Name))
		}
		isolated := workspaceMountMode(t) != "" || workspaceEnvDisabled(t) || len(readonlyMounts(t)) > 0
		if isolated && t.Container == nil && t.Script == nil && t.ContainerSet == nil {
			problems = append(problems, fmt.Errorf("argojob: template %q must declare workspace controls on each Pod leaf", t.Name))
		}
		// Defaults are merged by the controller after Build. Do not pretend a
		// leaf-only patch proves arbitrary late-added containers and mounts.
		if isolated && wf.Spec.TemplateDefaults != nil {
			problems = append(problems, fmt.Errorf("argojob: template %q requests workspace isolation with templateDefaults; declare the Pod fields on each leaf", t.Name))
		}
	})
	if d := wf.Spec.TemplateDefaults; d != nil {
		if workspaceMountMode(d) != "" || workspaceEnvDisabled(d) || len(readonlyMounts(d)) > 0 {
			problems = append(problems, errors.New("argojob: workspace isolation must be declared on templates, not templateDefaults"))
		}
		if tokenDisabled(d) && templateUsesCredentialChain(d) {
			problems = append(problems, errors.New("argojob: tokenless templateDefaults cannot invoke a credential chain"))
		}
		// Argo merges defaults after this boundary. Late mounts can expose
		// generated credentials or undo directory protection; late mirroring
		// can also re-enable an explicitly normalized same-name helper.
		templateContainers(d, func(c *corev1.Container) {
			if len(c.VolumeMounts) != 0 {
				problems = append(problems, errors.New("argojob: templateDefaults volumeMounts are not admitted; declare mounts on each Pod leaf"))
			}
		})
		if d.ContainerSet != nil && len(d.ContainerSet.VolumeMounts) != 0 {
			problems = append(problems, errors.New("argojob: templateDefaults containerSet volumeMounts are not admitted; declare mounts on each Pod leaf"))
		}
		for _, helpers := range [][]wfv1.UserContainer{d.Sidecars, d.InitContainers} {
			for _, helper := range helpers {
				if helper.MirrorVolumeMounts != nil && *helper.MirrorVolumeMounts {
					problems = append(problems, errors.New("argojob: templateDefaults mirrorVolumeMounts is not admitted; declare helpers on each Pod leaf"))
				}
			}
		}
	}
	return errors.Join(problems...)
}

// AI-INVARIANT: Argo 4.0.8 addOutputArtifactsVolumes mirrors every main mount
// into wait, even when the leaf has no output artifacts. Remove the server's
// credential mounts from wait and reassert readonly workspace protection after
// that mutation with a server-only strategic Pod patch. Repositories cannot
// supply patches.
func protectExecutorWorkspaceMounts(wf *wfv1.Workflow) error {
	var patchErr error
	walkTemplates(wf, func(t *wfv1.Template) {
		if patchErr != nil {
			return
		}
		patch := map[string]any{}
		if t.ContainerSet != nil {
			volumes, err := containerSetPodVolumes(wf, t)
			if err != nil {
				patchErr = err
				return
			}
			if len(volumes) > 0 {
				patch["volumes"] = volumes
			}
		}
		main := templateMainContainer(t)
		var mounts []any
		protectReadOnly := len(readonlyMounts(t)) > 0
		if main != nil {
			for _, m := range main.VolumeMounts {
				mirrorPath := path.Join(argoMainFilesystem, m.MountPath)
				if (m.Name == SecretsVolumeName && m.MountPath == SecretsMountPath) ||
					(m.Name == ReleaseTokenVolumeName && m.MountPath == ReleaseTokenMountPath) {
					// The wait executor has its own token. A readonly main-token
					// mirror can still authenticate to Vault, and its secret-root
					// mirror is writable under the same Pod UID. VolumeMounts merge
					// by mountPath, so delete only these exact generated paths.
					mounts = append(mounts, map[string]any{"mountPath": mirrorPath, "$patch": "delete"})
					continue
				}
				if protectReadOnly && m.ReadOnly {
					m.MountPath = mirrorPath
					mounts = append(mounts, m)
				}
			}
		}
		if len(mounts) > 0 {
			patch["containers"] = []map[string]any{{"name": argoWaitName, "volumeMounts": mounts}}
		}
		if len(patch) == 0 {
			return
		}
		encoded, err := json.Marshal(patch)
		if err != nil {
			patchErr = fmt.Errorf("argojob: encode readonly workspace executor policy: %w", err)
			return
		}
		t.PodSpecPatch = string(encoded)
	})
	return patchErr
}
