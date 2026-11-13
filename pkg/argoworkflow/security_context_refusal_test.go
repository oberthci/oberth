package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestSecurityContextPlatformFieldsRemainRefused(t *testing.T) {
	for field, security := range map[string]*corev1.SecurityContext{
		"appArmorProfile": {AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}},
		"seLinuxOptions":  {SELinuxOptions: &corev1.SELinuxOptions{Type: "unconfined_t"}},
		"procMount":       {ProcMount: ptr.To(corev1.UnmaskedProcMount)},
		"windowsOptions":  {WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(false)}},
	} {
		for _, shape := range []string{"main", "script", "init", "sidecar", "container-set", "template-defaults", "inline-dag", "inline-step"} {
			t.Run(shape+"/"+field, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				tmpl := &wf.Spec.Templates[0]
				container := tmpl.Container.DeepCopy()
				container.SecurityContext = security.DeepCopy()
				switch shape {
				case "main":
					tmpl.Container = container
				case "script":
					tmpl.Container = nil
					tmpl.Script = &wfv1.ScriptTemplate{Container: *container, Source: "true"}
				case "init":
					container.Name = "author-init"
					tmpl.InitContainers = []wfv1.UserContainer{{Container: *container}}
				case "sidecar":
					container.Name = "author-sidecar"
					tmpl.Sidecars = []wfv1.UserContainer{{Container: *container}}
				case "container-set":
					container.Name = "main"
					tmpl.Container = nil
					tmpl.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *container}}}
				case "template-defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{Container: container}
				case "inline-dag":
					tmpl.Container = nil
					tmpl.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: &wfv1.Template{Container: container}}}}
				case "inline-step":
					tmpl.Container = nil
					tmpl.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Inline: &wfv1.Template{Container: container}}}}}
				}
				if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "securityContext."+field) {
					t.Fatalf("platform field %s admitted or wrong refusal: %v", field, err)
				}
			})
		}
	}
}

func TestPodSecurityContextPlatformFieldsRemainRefused(t *testing.T) {
	for field, security := range map[string]*corev1.PodSecurityContext{
		"appArmorProfile": {AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}},
		"seLinuxOptions":  {SELinuxOptions: &corev1.SELinuxOptions{Type: "unconfined_t"}},
		"sysctls":         {Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_forward", Value: "1"}}},
		"windowsOptions":  {WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(false)}},
	} {
		for _, shape := range []string{"workflow", "template", "template-defaults", "inline"} {
			t.Run(shape+"/"+field, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				switch shape {
				case "workflow":
					wf.Spec.SecurityContext = security.DeepCopy()
				case "template":
					wf.Spec.Templates[0].SecurityContext = security.DeepCopy()
				case "template-defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{SecurityContext: security.DeepCopy()}
				case "inline":
					container := wf.Spec.Templates[0].Container
					wf.Spec.Templates[0].Container = nil
					wf.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: &wfv1.Template{Container: container, SecurityContext: security.DeepCopy()}}}}
				}
				if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "securityContext."+field) {
					t.Fatalf("platform field %s admitted or wrong refusal: %v", field, err)
				}
			})
		}
	}
}

func TestLegacyAppArmorPodAnnotationsRemainRefused(t *testing.T) {
	for _, shape := range []string{"workflow", "template", "template-defaults", "inline"} {
		t.Run(shape, func(t *testing.T) {
			wf := decodeAdmissible(t, nil)
			annotations := map[string]string{"container.apparmor.security.beta.kubernetes.io/main": "unconfined"}
			switch shape {
			case "workflow":
				wf.Spec.PodMetadata = &wfv1.Metadata{Annotations: annotations}
			case "template":
				wf.Spec.Templates[0].Metadata.Annotations = annotations
			case "template-defaults":
				wf.Spec.TemplateDefaults = &wfv1.Template{Metadata: wfv1.Metadata{Annotations: annotations}}
			case "inline":
				container := wf.Spec.Templates[0].Container
				wf.Spec.Templates[0].Container = nil
				wf.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: &wfv1.Template{Container: container, Metadata: wfv1.Metadata{Annotations: annotations}}}}}
			}
			if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "annotations") {
				t.Fatal("beta AppArmor profile annotation admitted or wrong refusal", err)
			}
		})
	}
}
