package argojob

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	nonrootUID         = int64(65534)
	nonrootTmpVolume   = "oberth-nonroot-tmp"
	nonrootPrepareName = "oberth-prepare-nonroot"
	// Argo v4.0.8 generated names/paths, verified against the real controller
	// by the test-only overlay contract. Do not import workflow/common here:
	// it would add executor implementation dependencies to the server binary.
	argoInitName       = "init"
	argoWaitName       = "wait"
	argoMainFilesystem = "/mainctrfs"
)

// applyNonrootLeaves runs after all ordinary environment/mount injection.
// AI-INVARIANT: authored security contexts and Pod patches remain forbidden.
// This server-only patch hardens Argo's generated init/wait as well as main.
// Only a fixed initializer receives UID0, with no token, source, PVC or host
// mount. It prepares fresh per-Pod EmptyDirs, never shared storage ownership.
func applyNonrootLeaves(workflow *wfv1.Workflow, config Config, request Request) error {
	names, err := argoworkflow.DeclaredNonrootTemplates(workflow)
	if err != nil || len(names) == 0 {
		return err
	}
	proof := request.nonrootProof
	if config.NonrootProfile != argoworkflow.NonrootStaticProfile || proof == nil ||
		proof.namespace != config.Namespace || proof.requestID != nonrootRequestIdentity(request) || time.Since(proof.verifiedAt) < 0 || time.Since(proof.verifiedAt) > 30*time.Second {
		return fmt.Errorf("argojob: nonroot controller support has not been established; selected leaves are disabled")
	}
	workflow.Annotations[nonrootProfileAnnotation] = proof.binding()
	workflow.Spec.Volumes = append(workflow.Spec.Volumes, corev1.Volume{
		Name:         nonrootTmpVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("8Gi"))}},
	})
	for i := range workflow.Spec.Templates {
		tmpl := &workflow.Spec.Templates[i]
		if !slices.Contains(names, tmpl.Name) {
			continue
		}
		tmpl.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser: ptr.To(nonrootUID), RunAsGroup: ptr.To(nonrootUID), RunAsNonRoot: ptr.To(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
		container := tmpl.Container
		if tmpl.Script != nil {
			container = &tmpl.Script.Container
		}
		container.SecurityContext = nonrootContainerSecurity(nonrootUID)
		mounts := make([]corev1.VolumeMount, 0, len(container.VolumeMounts))
		for _, mount := range container.VolumeMounts {
			if mount.Name == CacheVolumeName || mount.Name == stepTmpVolumeName {
				continue
			}
			// Pinned tools and source are inputs. Only the existing, separately
			// collected per-run artifacts mount may be written for publication.
			mount.ReadOnly = mount.Name != ArtifactsVolumeName
			mounts = append(mounts, mount)
		}
		container.VolumeMounts = append(mounts, corev1.VolumeMount{Name: nonrootTmpVolume, MountPath: "/tmp"})
		container.Env = overrideEnvironment(container.Env, []corev1.EnvVar{
			{Name: "TMPDIR", Value: "/tmp"}, {Name: "GOTMPDIR", Value: "/tmp"},
			{Name: "GOCACHE", Value: "/tmp/gobuild"}, {Name: "GOMODCACHE", Value: "/tmp/gomod"},
			{Name: "OBERTH_CACHE_DIR", Value: "/tmp/cache"}, {Name: "HOME", Value: "/tmp"},
			{Name: "XDG_CACHE_HOME", Value: "/tmp/cache"},
		})
		patch, err := nonrootPodPatch(tmpl, config.SourceSeedImage)
		if err != nil {
			return err
		}
		tmpl.PodSpecPatch = string(patch)
		if err := argoworkflow.ValidateNonrootTemplateSize(tmpl); err != nil {
			return err
		}
	}
	return nil
}

func nonrootContainerSecurity(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: ptr.To(uid), RunAsGroup: ptr.To(uid), RunAsNonRoot: ptr.To(uid != 0),
		AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func nonrootPodPatch(tmpl *wfv1.Template, seedImage string) ([]byte, error) {
	if strings.TrimSpace(seedImage) == "" {
		return nil, fmt.Errorf("argojob: nonroot preparation requires the server's pinned source seed image")
	}
	if err := periapsis.ValidateRunnerImage(seedImage); err != nil {
		return nil, fmt.Errorf("argojob: invalid nonroot preparation image: %w", err)
	}
	prepareMounts := []corev1.VolumeMount{
		{Name: nonrootTmpVolume, MountPath: "/prepare/tmp"},
		{Name: "var-run-argo", MountPath: "/prepare/argo"},
		{Name: "tmp-dir-argo", MountPath: "/prepare/executor"},
	}
	// The first init owns the root:root0755 executor parent. A replay may see
	// its own partially prepared directory, but must never follow a symlink
	// or adopt another owner's entry. The parent is not writable by main.
	command := "if [ ! -e /prepare/executor/0 ]; then mkdir /prepare/executor/0; fi; " +
		"test -d /prepare/executor/0 && test ! -L /prepare/executor/0 && test -O /prepare/executor/0 && " +
		"chmod 1777 /prepare/tmp /prepare/argo /prepare/executor/0"
	if tmpl.Script != nil {
		prepareMounts = append(prepareMounts, corev1.VolumeMount{Name: "argo-staging", MountPath: "/prepare/staging"})
		command += " /prepare/staging"
	}
	prepare := corev1.Container{
		Name: nonrootPrepareName, Image: seedImage,
		Command:         []string{"/bin/sh", "-ec", command},
		SecurityContext: nonrootContainerSecurity(0), VolumeMounts: prepareMounts,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
	}
	main := tmpl.Container
	if tmpl.Script != nil {
		main = &tmpl.Script.Container
	}
	// Argo normally mirrors every main mount into wait read-write. These
	// artifact-free leaves need only read access, including to source/tools.
	waitMounts := make([]corev1.VolumeMount, 0, len(main.VolumeMounts))
	for _, mount := range main.VolumeMounts {
		mount.MountPath = argoMainFilesystem + mount.MountPath
		mount.ReadOnly = true
		waitMounts = append(waitMounts, mount)
	}
	if tmpl.Script != nil {
		waitMounts = append(waitMounts, corev1.VolumeMount{Name: "argo-staging", MountPath: "/mainctrfs/argo/staging", ReadOnly: true})
	}
	// Give every SecurityContext field replacement semantics. A nested
	// $patch:replace map is discarded by strategic merge when the original
	// context is absent, so explicitly clear all other fields instead. The
	// reflection guard in tests fails if the Kubernetes type grows a new field.
	security := map[string]any{
		"runAsUser": nonrootUID, "runAsGroup": nonrootUID, "runAsNonRoot": true,
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
		"privileged": nil, "seLinuxOptions": nil, "windowsOptions": nil,
		"procMount": nil, "appArmorProfile": nil,
		"capabilities":   map[string]any{"add": nil, "drop": []string{"ALL"}},
		"seccompProfile": map[string]any{"type": string(corev1.SeccompProfileTypeRuntimeDefault), "localhostProfile": nil},
	}
	volumes := []map[string]any{
		{"name": "var-run-argo", "emptyDir": map[string]string{"sizeLimit": "128Mi"}},
		{"name": "tmp-dir-argo", "emptyDir": map[string]string{"sizeLimit": "64Mi"}},
	}
	if tmpl.Script != nil {
		volumes = append(volumes, map[string]any{"name": "argo-staging", "emptyDir": map[string]string{"sizeLimit": "16Mi"}})
	}
	patch := map[string]any{
		"securityContext": map[string]any{
			"runAsUser": nonrootUID, "runAsGroup": nonrootUID, "runAsNonRoot": true,
			"seLinuxOptions": nil, "windowsOptions": nil, "supplementalGroups": nil,
			"supplementalGroupsPolicy": nil, "fsGroup": nil, "sysctls": nil,
			"fsGroupChangePolicy": nil, "appArmorProfile": nil, "seLinuxChangePolicy": nil,
			"seccompProfile": map[string]any{"type": string(corev1.SeccompProfileTypeRuntimeDefault), "localhostProfile": nil},
		},
		"volumes":                         volumes,
		"$setElementOrder/initContainers": []map[string]string{{"name": nonrootPrepareName}, {"name": argoInitName}},
		"initContainers": []any{prepare, map[string]any{
			"name": argoInitName, "securityContext": security,
			"volumeMounts": []corev1.VolumeMount{{Name: nonrootTmpVolume, MountPath: "/tmp"}},
		}},
		"containers": []map[string]any{{"name": argoWaitName, "securityContext": security, "volumeMounts": waitMounts}},
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("argojob: encode nonroot executor policy: %w", err)
	}
	return encoded, nil
}
