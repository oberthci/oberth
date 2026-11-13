package argojob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestExecutorIdentityAPIDefaultedHelpers(t *testing.T) {
	for _, helper := range []corev1.Container{executorWaiterContainer(defaultSourceSeedImage, executorWaiterResources(false)), nonrootPrepareContainer(false, defaultSourceSeedImage), nonrootPrepareContainer(true, defaultSourceSeedImage)} {
		t.Run(helper.Name+"/"+string(rune(len(helper.VolumeMounts)+'0')), func(t *testing.T) {
			actual := *helper.DeepCopy()
			actual.ImagePullPolicy = corev1.PullAlways
			actual.TerminationMessagePath = corev1.TerminationMessagePathDefault
			actual.TerminationMessagePolicy = corev1.TerminationMessageReadFile
			actual.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
			actual.Resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
			actual.SecurityContext.Privileged = ptr.To(false)
			actual.SecurityContext.ProcMount = ptr.To(corev1.DefaultProcMount)
			actual.SecurityContext.RunAsUser, actual.SecurityContext.RunAsGroup = ptr.To(int64(1000)), ptr.To(int64(1000))
			actual.SecurityContext.RunAsNonRoot = ptr.To(true)
			actual.SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}
			for i, j := 0, len(actual.VolumeMounts)-1; i < j; i, j = i+1, j-1 {
				actual.VolumeMounts[i], actual.VolumeMounts[j] = actual.VolumeMounts[j], actual.VolumeMounts[i]
			}
			if !sameServerContainer(actual, helper) {
				t.Fatal("safe API/admission changes rejected")
			}
		})
	}
}

func TestExecutorIdentityStrictHelperFields(t *testing.T) {
	mutations := map[string]func(*corev1.Container){
		"name":    func(c *corev1.Container) { c.Name = "other" },
		"image":   func(c *corev1.Container) { c.Image = "other" },
		"command": func(c *corev1.Container) { c.Command = []string{"other"} },
		"args":    func(c *corev1.Container) { c.Args = []string{"other"} },
		"env":     func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "PATH", Value: "/loot"}} },
		"envFrom": func(c *corev1.Container) { c.EnvFrom = []corev1.EnvFromSource{{Prefix: "LOOT"}} },
		"lifecycle": func(c *corev1.Container) {
			c.Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"other"}}}}
		},
		"liveness":      func(c *corev1.Container) { c.LivenessProbe = &corev1.Probe{} },
		"readiness":     func(c *corev1.Container) { c.ReadinessProbe = &corev1.Probe{} },
		"startup":       func(c *corev1.Container) { c.StartupProbe = &corev1.Probe{} },
		"restartPolicy": func(c *corev1.Container) { c.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways) },
		"workingDir":    func(c *corev1.Container) { c.WorkingDir = "/loot" },
		"devices": func(c *corev1.Container) {
			c.VolumeDevices = []corev1.VolumeDevice{{Name: "loot", DevicePath: "/dev/loot"}}
		},
		"mount-name":        func(c *corev1.Container) { c.VolumeMounts[0].Name = "loot" },
		"mount-path":        func(c *corev1.Container) { c.VolumeMounts[0].MountPath = "/loot" },
		"mount-readOnly":    func(c *corev1.Container) { c.VolumeMounts[0].ReadOnly = !c.VolumeMounts[0].ReadOnly },
		"mount-subPath":     func(c *corev1.Container) { c.VolumeMounts[0].SubPath = "loot" },
		"mount-subPathExpr": func(c *corev1.Container) { c.VolumeMounts[0].SubPathExpr = "$(LOOT)" },
		"mount-extra": func(c *corev1.Container) {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "loot", MountPath: "/loot"})
		},
		"mount-missing":       func(c *corev1.Container) { c.VolumeMounts = nil },
		"security-missing":    func(c *corev1.Container) { c.SecurityContext = nil },
		"security-privileged": func(c *corev1.Container) { c.SecurityContext.Privileged = ptr.To(true) },
		"security-escalation": func(c *corev1.Container) { c.SecurityContext.AllowPrivilegeEscalation = ptr.To(true) },
		"security-writable":   func(c *corev1.Container) { c.SecurityContext.ReadOnlyRootFilesystem = ptr.To(false) },
		"security-capability": func(c *corev1.Container) { c.SecurityContext.Capabilities.Add = []corev1.Capability{"SYS_ADMIN"} },
		"security-drop":       func(c *corev1.Container) { c.SecurityContext.Capabilities.Drop = nil },
		"security-seccomp":    func(c *corev1.Container) { c.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined },
		"security-proc":       func(c *corev1.Container) { c.SecurityContext.ProcMount = ptr.To(corev1.UnmaskedProcMount) },
		"security-apparmor": func(c *corev1.Container) {
			c.SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
		},
	}
	for _, helper := range []corev1.Container{executorWaiterContainer(defaultSourceSeedImage, executorWaiterResources(false)), nonrootPrepareContainer(false, defaultSourceSeedImage)} {
		t.Run(helper.Name, func(t *testing.T) {
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					actual := *helper.DeepCopy()
					mutate(&actual)
					if validServerContainer(actual, defaultSourceSeedImage, true) {
						t.Fatal("modified helper accepted")
					}
				})
			}
			t.Run("position", func(t *testing.T) {
				if validServerContainer(helper, defaultSourceSeedImage, false) {
					t.Fatal("helper accepted outside initContainers")
				}
			})
		})
	}
}

func TestExecutorIdentityRejectsResourceScratchAlias(t *testing.T) {
	for _, shape := range []string{"container", "resource"} {
		for _, attack := range []string{"manifest-fifo", "runtime-alias", "runtime-subPath", "runtime-duplicate", "scratch-device", "ephemeral", "staging-alias", "runtime-writable", "runtime-propagation", "native-resource-helper"} {
			if shape == "container" && attack == "native-resource-helper" {
				continue
			}
			t.Run(shape+"/"+attack, func(t *testing.T) {
				controller, _, pod, _ := executorRetryFixture(t)
				pod.Spec = executorEnvironmentPod(t, shape, "init", nil)
				c := corev1.Container{Name: "plant", Image: "authored-image", Command: []string{"sh", "-c", "mkdir -p /x/0; mkfifo -m 666 /var/run/argo/f; ln -s /var/run/argo/f /x/0/manifest.yaml"}, VolumeMounts: []corev1.VolumeMount{{Name: "var-run-argo", MountPath: "/var/run/argo", ReadOnly: true}}}
				switch attack {
				case "staging-alias":
					c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "argo-staging", MountPath: "/x"})
				case "runtime-writable":
					c.VolumeMounts[0].ReadOnly = false
				case "runtime-propagation":
					c.VolumeMounts[0].MountPropagation = ptr.To(corev1.MountPropagationHostToContainer)
				case "native-resource-helper": // The native writable argoexec mount is unsafe on a resource helper too.
				case "manifest-fifo":
					c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "tmp-dir-argo", MountPath: "/x"})
				case "runtime-alias":
					c.VolumeMounts[0].MountPath = "/x"
				case "runtime-subPath":
					c.VolumeMounts[0].SubPath = "x"
				case "runtime-duplicate":
					c.VolumeMounts = append(c.VolumeMounts, c.VolumeMounts[0])
				case "scratch-device":
					c.VolumeDevices = []corev1.VolumeDevice{{Name: "tmp-dir-argo", DevicePath: "/dev/x"}}
				case "ephemeral":
					pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", VolumeMounts: []corev1.VolumeMount{{Name: "tmp-dir-argo", MountPath: "/x"}}}}}
				}
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
				if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, shape == "resource"); err == nil {
					t.Fatal("authored scratch alias accepted")
				}
			})
		}
	}
}

func TestExecutorIdentityFullBuildControllerMerge(t *testing.T) {
	for _, shape := range []string{"nonroot-container", "nonroot-script", "ordinary", "helpers"} {
		t.Run(shape, func(t *testing.T) {
			var wfSource = nonrootTestDocument
			if shape == "ordinary" || shape == "helpers" {
				wfSource = greedyDocument
			}
			wf, err := Build(testConfig(), testRequest(pipeline.TriggerCI, wfSource))
			if err != nil {
				t.Fatal(err)
			}
			var pod *corev1.Pod
			switch shape {
			case "nonroot-container", "nonroot-script":
				wf = nonrootBuiltFixture(t, shape == "nonroot-script")
				fixture := "container-before-patch.json"
				if shape == "nonroot-script" {
					fixture = "script-before-patch.json"
				}
				pod = nonrootApplyControllerFixture(t, nonrootControllerFixture(t, fixture), wf)
			default:
				if shape == "helpers" {
					// Full Build with authored helpers, then the actual controller's
					// pre-patch helper mounts and final post-patch sidecar mount.
					request := testRequest(pipeline.TriggerCI, `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  templates:
  - name: main
    initContainers:
    - name: prepare
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
    sidecars:
    - name: companion
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`)
					wf, err = Build(testConfig(), request)
					if err != nil {
						t.Fatal(err)
					}
				}
				base := executorEnvironmentPod(t, "container", "init", nil)
				// Remove the fixture's server waiter: Build's patch must supply it.
				var init []corev1.Container
				for _, c := range base.InitContainers {
					if c.Name != executorTokenWaiter {
						init = append(init, c)
					}
				}
				base.InitContainers = init
				for _, c := range wf.Spec.Templates[0].InitContainers {
					c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
					base.InitContainers = append(base.InitContainers, c.Container)
				}
				for _, c := range wf.Spec.Templates[0].Sidecars {
					base.Containers = append(base.Containers, c.Container)
				}
				base = applyCredentialWaitPatch(t, base, wf.Spec.Templates[0].PodSpecPatch)
				for i := range base.Containers {
					base.Containers[i].VolumeMounts = append(base.Containers[i].VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
				}
				pod = &corev1.Pod{Spec: base}
			}
			if err := validateExecutorTokenTarget(pod, defaultSourceSeedImage, false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecutorIdentityResourceManifestAdmission(t *testing.T) {
	for _, shape := range []string{"container", "resource"} {
		for _, attack := range []bool{false, true} {
			name := shape + "/control"
			if attack {
				name = shape + "/manifest-fifo"
			}
			t.Run(name, func(t *testing.T) {
				source := greedyDocument
				if shape == "resource" {
					source = operatorResourceDocument
				}
				wf, err := argoworkflow.Decode([]byte(source))
				if err != nil {
					t.Fatal(err)
				}
				helper := corev1.Container{Name: "plant", Image: "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"sh", "-c", "true"}}
				if attack {
					helper.Command = []string{"sh", "-c", "mkdir -p /x/0; mkfifo -m 666 /var/run/argo/f; ln -s /var/run/argo/f /x/0/manifest.yaml"}
					helper.VolumeMounts = []corev1.VolumeMount{{Name: "tmp-dir-argo", MountPath: "/x"}}
				}
				wf.Spec.Templates[0].InitContainers = []wfv1.UserContainer{{Container: helper}}
				body, err := json.Marshal(wf)
				if err != nil {
					t.Fatal(err)
				}
				request := testRequest(pipeline.TriggerCI, string(body))
				if shape == "resource" {
					request.Capability = operatorCapability()
				}
				_, err = Build(testConfig(), request)
				if attack {
					if err == nil || !strings.Contains(err.Error(), "reserved executor identity volume") {
						t.Fatalf("resource-manifest scratch alias admitted: %v", err)
					}
				} else if shape == "resource" {
					if err == nil || !strings.Contains(err.Error(), "resource templates may not declare initContainers or sidecars") {
						t.Fatalf("resource helper admitted: %v", err)
					}
				} else if err != nil {
					t.Fatalf("admission control failed: %v", err)
				}
			})
		}
	}
}

// Replay public Pod observations after final guard refinements without creating
// further Workflows. The live test writes these only after real completion.
func TestExecutorIdentityObservedAPIPods(t *testing.T) {
	dir := filepath.Join("testdata", "executor-identity-argo4.0.8")
	for _, shape := range []string{"ordinary", "resource"} {
		t.Run(shape, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(dir, shape+"-pods.json"))
			if err != nil {
				t.Fatal(err)
			}
			var pods corev1.PodList
			if err = json.Unmarshal(body, &pods); err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, pod := range pods.Items {
				if pod.Labels["oberth.ci/role"] == "executor-identity" {
					continue
				}
				checked++
				if err := validateExecutorTokenTarget(&pod, defaultSourceSeedImage, shape == "resource"); err != nil {
					t.Fatal(err)
				}
			}
			if checked != 1 {
				t.Fatalf("checked %d Pods", checked)
			}
		})
	}
}
