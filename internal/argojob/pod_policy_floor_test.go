package argojob

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func podPolicyBaseline() *corev1.Pod {
	floor := func() *corev1.SecurityContext {
		return &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		}
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"workflows.argoproj.io/workflow": "test"}}, Spec: corev1.PodSpec{
		Containers:          []corev1.Container{{Name: "main", SecurityContext: floor()}},
		InitContainers:      []corev1.Container{{Name: "init", SecurityContext: floor()}},
		EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", SecurityContext: floor()}}},
	}}
}

// Each mutation changes one floor requirement. The same hostile shape is
// evaluated for every container kind against the actual rendered CEL.
func TestPodPolicySecurityFloor(t *testing.T) {
	accepts := renderedPodPolicy(t)
	for _, kind := range []string{"main", "init", "ephemeral"} {
		context := func(p *corev1.Pod) **corev1.SecurityContext {
			switch kind {
			case "init":
				return &p.Spec.InitContainers[0].SecurityContext
			case "ephemeral":
				return &p.Spec.EphemeralContainers[0].SecurityContext
			default:
				return &p.Spec.Containers[0].SecurityContext
			}
		}
		for name, mutate := range map[string]func(**corev1.SecurityContext){
			"host-process": func(s **corev1.SecurityContext) {
				(*s).WindowsOptions = &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(true)}
			},
			"privileged":           func(s **corev1.SecurityContext) { (*s).Privileged = ptr.To(true) },
			"added-capability":     func(s **corev1.SecurityContext) { (*s).Capabilities.Add = []corev1.Capability{"SYS_ADMIN"} },
			"missing-drop":         func(s **corev1.SecurityContext) { (*s).Capabilities.Drop = nil },
			"partial-drop":         func(s **corev1.SecurityContext) { (*s).Capabilities.Drop = []corev1.Capability{"NET_RAW"} },
			"missing-capabilities": func(s **corev1.SecurityContext) { (*s).Capabilities = nil },
			"escalation":           func(s **corev1.SecurityContext) { (*s).AllowPrivilegeEscalation = ptr.To(true) },
			"missing-escalation":   func(s **corev1.SecurityContext) { (*s).AllowPrivilegeEscalation = nil },
			"unconfined":           func(s **corev1.SecurityContext) { (*s).SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined },
			"missing-seccomp":      func(s **corev1.SecurityContext) { (*s).SeccompProfile = nil },
			"missing-context":      func(s **corev1.SecurityContext) { *s = nil },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				p := podPolicyBaseline()
				if !accepts(p) {
					t.Fatal("floor baseline refused")
				}
				mutate(context(p))
				if accepts(p) {
					t.Fatal("hostile container admitted")
				}
			})
		}
		t.Run(kind+"/host-port", func(t *testing.T) {
			p := podPolicyBaseline()
			ports := []corev1.ContainerPort{{ContainerPort: 8080, HostPort: 8080}}
			switch kind {
			case "init":
				p.Spec.InitContainers[0].Ports = ports
			case "ephemeral":
				p.Spec.EphemeralContainers[0].Ports = ports
			default:
				p.Spec.Containers[0].Ports = ports
			}
			if accepts(p) {
				t.Fatal("host port admitted")
			}
			ports[0].HostPort = 0
			if !accepts(p) {
				t.Fatal("ordinary container port refused")
			}
		})
		t.Run(kind+"/pod-seccomp", func(t *testing.T) {
			p := podPolicyBaseline()
			(*context(p)).SeccompProfile = nil
			p.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
			if !accepts(p) {
				t.Fatal("inherited RuntimeDefault refused")
			}
			p.Spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeLocalhost
			p.Spec.SecurityContext.SeccompProfile.LocalhostProfile = ptr.To("profiles/pipeline.json")
			if !accepts(p) {
				t.Fatal("inherited Localhost refused")
			}
			p.Spec.SecurityContext.SeccompProfile.LocalhostProfile = nil
			p.Spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
			if accepts(p) {
				t.Fatal("inherited Unconfined admitted")
			}
			(*context(p)).SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
			if accepts(p) {
				t.Fatal("Pod Unconfined admitted despite safe container override")
			}
			p.Spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeRuntimeDefault
			(*context(p)).SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
			if accepts(p) {
				t.Fatal("unsafe container override admitted")
			}
		})
	}
	for _, profile := range []corev1.SeccompProfileType{corev1.SeccompProfileTypeRuntimeDefault, corev1.SeccompProfileTypeLocalhost} {
		t.Run("allowed/"+string(profile), func(t *testing.T) {
			p := podPolicyBaseline()
			for _, s := range []*corev1.SecurityContext{p.Spec.Containers[0].SecurityContext, p.Spec.InitContainers[0].SecurityContext, p.Spec.EphemeralContainers[0].SecurityContext} {
				s.RunAsUser = ptr.To(int64(0))
				s.ReadOnlyRootFilesystem = ptr.To(false)
				s.Capabilities.Add = []corev1.Capability{"NET_BIND_SERVICE"}
				s.SeccompProfile.Type = profile
				if profile == corev1.SeccompProfileTypeLocalhost {
					s.SeccompProfile.LocalhostProfile = ptr.To("profiles/pipeline.json")
				}
			}
			if !accepts(p) {
				t.Fatal("root, writable root and allowed capability/profile refused")
			}
		})
	}
}

func TestPodPolicySeccompTiers(t *testing.T) {
	accepts := renderedPodPolicy(t)
	for _, kind := range []string{"main", "init", "ephemeral"} {
		for _, step := range []bool{false, true} {
			t.Run(kind+"/step="+fmt.Sprint(step), func(t *testing.T) {
				p := podPolicyBaseline()
				if !step {
					p.Labels = nil
				}
				var s *corev1.SecurityContext
				switch kind {
				case "main":
					s = p.Spec.Containers[0].SecurityContext
				case "init":
					s = p.Spec.InitContainers[0].SecurityContext
				case "ephemeral":
					s = p.Spec.EphemeralContainers[0].SecurityContext
				}
				s.SeccompProfile = nil
				if accepts(p) == step {
					t.Fatal("unset seccomp did not distinguish step and other Pods")
				}
				s.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
				if accepts(p) {
					t.Fatal("Unconfined container admitted")
				}
				s.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: ptr.To("profiles/test.json")}
				if !accepts(p) {
					t.Fatal("Localhost refused")
				}
				s.WindowsOptions = &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(true)}
				if accepts(p) {
					t.Fatal("container hostProcess admitted")
				}

			})
		}
	}
	for _, step := range []bool{false, true} {
		t.Run("pod/step="+fmt.Sprint(step), func(t *testing.T) {
			p := podPolicyBaseline()
			if !step {
				p.Labels = nil
			}
			p.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
			// Pod-level Unconfined is refused even when every container overrides it.
			if accepts(p) {
				t.Fatal("incorrect Pod-level seccomp rule")
			}
			p.Spec.SecurityContext = &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(true)}}
			if accepts(p) {
				t.Fatal("Pod hostProcess admitted")
			}
		})
	}
}

func TestPodPolicyAllowsMetadataUpdatesOnly(t *testing.T) {
	p := podPolicyBaseline()
	p.Spec.Containers[0].SecurityContext = nil
	if renderedPodPolicyRequest(t, "CREATE", "")(p) {
		t.Fatal("unsafe new Pod admitted")
	}
	if !renderedPodPolicyRequest(t, "UPDATE", "")(p) {
		t.Fatal("existing Pod metadata patch rejudged")
	}
	if renderedPodPolicyRequest(t, "UPDATE", "ephemeralcontainers")(p) {
		t.Fatal("ephemeral subresource bypassed policy")
	}
}

func TestPodPolicyStepLabelCannotBeAuthored(t *testing.T) {
	for _, where := range []string{"podMetadata", "template", "defaults"} {
		t.Run(where, func(t *testing.T) {
			wf, err := argoworkflow.Decode([]byte(admissibleSecurityDocument))
			if err != nil {
				t.Fatal(err)
			}
			labels := map[string]string{"workflows.argoproj.io/workflow": "attempted"}
			switch where {
			case "podMetadata":
				wf.Spec.PodMetadata = &wfv1.Metadata{Labels: labels}
			case "template":
				wf.Spec.Templates[0].Metadata.Labels = labels
			case "defaults":
				wf.Spec.TemplateDefaults = &wfv1.Template{Metadata: wfv1.Metadata{Labels: labels}}
			}
			if err := argoworkflow.Admit(wf, argoworkflow.Policy{}); err == nil || !strings.Contains(err.Error(), "workflows.argoproj.io/") {
				t.Fatalf("reserved workflow label admitted: %v", err)
			}
		})
	}
}
