package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func executorAdmissionLeaf(t *testing.T, wf *wfv1.Workflow, carrier string) *wfv1.Template {
	t.Helper()
	leaf := wf.Spec.Templates[0].DeepCopy()
	switch carrier {
	case "direct":
		return &wf.Spec.Templates[0]
	case "defaults":
		wf.Spec.TemplateDefaults = leaf
		return leaf
	case "containerSet":
		leaf.Container.Name = "main"
		leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *leaf.Container}}}
		leaf.Container = nil
		wf.Spec.Templates[0] = *leaf
		return &wf.Spec.Templates[0]
	case "inline-dag":
		wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "call", Inline: leaf}}}}
		return leaf
	case "inline-steps":
		wf.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "call", Inline: leaf}}}}}
		return leaf
	case "workflow-hook", "task-hook", "step-hook":
		leaf.Name = "hook"
		wf.Spec.Templates = append(wf.Spec.Templates, *leaf)
		hooks := wfv1.LifecycleHooks{"exit": {Template: "hook"}}
		if carrier == "workflow-hook" {
			wf.Spec.Hooks = hooks
		} else {
			plain := wf.Spec.Templates[0]
			plain.Name = "plain"
			wf.Spec.Templates = append(wf.Spec.Templates, plain)
			if carrier == "task-hook" {
				wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "call", Template: "plain", Hooks: hooks}}}}
			} else {
				wf.Spec.Templates[0] = wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "call", Template: "plain", Hooks: hooks}}}}}
			}
		}
		return &wf.Spec.Templates[1]
	default:
		t.Fatalf("unknown carrier %s", carrier)
		return nil
	}
}

func TestAdmitExecutorIdentityGuardsCoverEveryTemplateCarrier(t *testing.T) {
	for _, carrier := range []string{"direct", "defaults", "containerSet", "inline-dag", "inline-steps", "workflow-hook", "task-hook", "step-hook"} {
		for _, guard := range []string{"helper-name", "env-name", "volume-name", "mount-name", "mount-path"} {
			t.Run(carrier+"/"+guard, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				image := wf.Spec.Templates[0].Container.Image
				leaf := executorAdmissionLeaf(t, wf, carrier)
				container := leaf.Container
				if leaf.ContainerSet != nil {
					container = &leaf.ContainerSet.Containers[0].Container
				}
				want := ""
				switch guard {
				case "helper-name":
					leaf.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "wait", Image: image}}}
					want = "reserved for the Argo executor"
				case "env-name":
					container.Env = []corev1.EnvVar{{Name: "{{='ARGO_'+'PROGRESS_PATCH_TICK_DURATION'}}", Value: "0"}}
					want = "environment names must be literal"
				case "volume-name":
					leaf.Volumes = []corev1.Volume{{Name: ExecutorIdentityVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}}}
					want = "reserved executor identity volume"
				case "mount-name":
					container.VolumeMounts = []corev1.VolumeMount{{Name: ExecutorIdentityVolumeName, MountPath: "/identity"}}
					want = "reserved executor identity volume"
				case "mount-path":
					container.VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: ExecutorIdentityMountPath}}
					want = "reserved server helper path"
				}
				if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("executor identity guard bypassed via %s: %v", carrier, err)
				}
			})
		}
	}
}

func TestAdmitRefusesResourceExecutorTokenTheft(t *testing.T) {
	for _, carrier := range []string{"sidecar", "initContainer", "defaults"} {
		t.Run(carrier, func(t *testing.T) {
			wf, err := Decode([]byte(capabilityResourceDocument))
			if err != nil {
				t.Fatal(err)
			}
			image := decodeAdmissible(t, nil).Spec.Templates[0].Container.Image
			attacker := wfv1.UserContainer{Container: corev1.Container{Name: "wait", Image: image,
				VolumeMounts: []corev1.VolumeMount{{Name: ExecutorIdentityVolumeName, MountPath: ExecutorIdentityMountPath}},
				Env: []corev1.EnvVar{{Name: "KUBECONFIG", Value: ExecutorIdentityMountPath + "/kubeconfig"},
					{Name: "{{='ARGO_'+'PROGRESS_PATCH_TICK_DURATION'}}", Value: "0"},
					{Name: "{{='ARGO_'+'PROGRESS_FILE_TICK_DURATION'}}", Value: "0"}}}}
			leaf := &wf.Spec.Templates[0]
			if carrier == "defaults" {
				wf.Spec.TemplateDefaults = &wfv1.Template{}
				leaf = wf.Spec.TemplateDefaults
			}
			if carrier == "initContainer" {
				leaf.InitContainers = []wfv1.UserContainer{attacker}
			} else {
				leaf.Sidecars = []wfv1.UserContainer{attacker}
			}
			err = Admit(wf, capabilityPolicyForTest(t))
			for _, want := range []string{"reserved for the Argo executor", "reserved executor identity volume", "environment names must be literal"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("resource executor theft not refused at %s: %v", want, err)
				}
			}
		})
	}
}

func TestAdmitExecutorContainerNamesAndTemplatedValues(t *testing.T) {
	for _, kind := range []string{"container", "script", "containerSet", "sidecar", "initContainer"} {
		for _, name := range []string{"init", "wait", "main", "{{inputs.parameters.name}}", "helper"} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				leaf := &wf.Spec.Templates[0]
				container := *leaf.Container
				container.Name = name
				container.Env = []corev1.EnvVar{{Name: "CUSTOM", Value: "{{workflow.name}}"}}
				switch kind {
				case "container":
					leaf.Container = &container
				case "script":
					leaf.Container = nil
					leaf.Script = &wfv1.ScriptTemplate{Container: container, Source: "true"}
				case "containerSet":
					leaf.Container = nil
					leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: container}}}
				case "sidecar":
					leaf.Sidecars = []wfv1.UserContainer{{Container: container}}
				case "initContainer":
					leaf.InitContainers = []wfv1.UserContainer{{Container: container}}
				}
				refused := name == "init" || name == "wait" || strings.Contains(name, "{{") || (name == "main" && (kind == "sidecar" || kind == "initContainer"))
				err := Admit(wf, Policy{})
				if (err != nil) != refused {
					t.Fatalf("name=%s kind=%s refusal=%v: %v", name, kind, refused, err)
				}
			})
		}
	}
}

func TestAdmitExecutorIdentityVolumeAliases(t *testing.T) {
	for _, mountPath := range []string{ExecutorIdentityMountPath, ExecutorIdentityMountPath + "/kubeconfig", "/run/secrets/kubernetes.io/serviceaccount", "/var/run/secrets/kubernetes.io/serviceaccount/../serviceaccount/token"} {
		t.Run(mountPath, func(t *testing.T) {
			wf := decodeAdmissible(t, nil)
			wf.Spec.Templates[0].Container.VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: mountPath}}
			if err := Admit(wf, Policy{}); err == nil {
				t.Fatal("executor identity mount path admitted")
			}
		})
	}
	for _, name := range []string{ExecutorIdentityVolumeName, "{{inputs.parameters.volume}}"} {
		for _, kind := range []string{"volume", "claim"} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				if kind == "volume" {
					wf.Spec.Volumes = []corev1.Volume{{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}}}
				} else {
					wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}}}
				}
				if err := Admit(wf, Policy{}); err == nil {
					t.Fatal("executor identity volume alias admitted")
				}
			})
		}
	}
}
