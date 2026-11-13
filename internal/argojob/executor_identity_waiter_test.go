package argojob

import (
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestExecutorIdentityRejectsMergedWaiterFields(t *testing.T) {
	for _, shape := range []string{"container", "resource"} {
		for _, addition := range []string{"env-only", "lifecycle-only", "sketch", "envFrom", "workingDir", "livenessProbe", "readinessProbe", "startupProbe", "restartPolicy", "volumeDevices", "extra-mount", "missing-mount", "mount-subPath", "security", "ports", "resizePolicy", "stdin", "tty", "duplicate"} {
			t.Run(shape+"/"+addition, func(t *testing.T) {
				controller, _, pod, _ := executorRetryFixture(t)
				pod.Spec = executorEnvironmentPod(t, shape, "init", nil)
				// Replace the server waiter with the authored namesake, then
				// apply the real server strategic patch again, as Argo does.
				attacker := corev1.Container{Name: executorTokenWaiter, Image: "authored-image", Command: []string{"/bin/true"}}
				switch addition {
				case "env-only":
					attacker.Env = []corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/loot/x.so"}}
				case "lifecycle-only":
					attacker.Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "copy-identity"}}}}
				case "sketch":
					attacker.Env = []corev1.EnvVar{{Name: "PATH", Value: "/loot"}, {Name: "LD_PRELOAD", Value: "/loot/x.so"}}
					attacker.Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "copy-identity"}}}}
					attacker.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
					attacker.VolumeMounts = []corev1.VolumeMount{{Name: "loot", MountPath: "/loot"}}
				case "envFrom":
					attacker.EnvFrom = []corev1.EnvFromSource{{Prefix: "AUTHOR_"}}
				case "workingDir":
					attacker.WorkingDir = "/loot"
				case "livenessProbe":
					attacker.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"copy-identity"}}}}
				case "readinessProbe":
					attacker.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"copy-identity"}}}}
				case "startupProbe":
					attacker.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"copy-identity"}}}}
				case "restartPolicy":
					attacker.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
				case "volumeDevices":
					attacker.VolumeDevices = []corev1.VolumeDevice{{Name: "loot", DevicePath: "/dev/loot"}}
				case "extra-mount":
					attacker.VolumeMounts = []corev1.VolumeMount{{Name: "loot", MountPath: "/loot"}}
				case "ports":
					attacker.Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
				case "resizePolicy":
					attacker.ResizePolicy = []corev1.ContainerResizePolicy{{ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.RestartContainer}}
				case "stdin":
					attacker.Stdin = true
				case "tty":
					attacker.TTY = true
				}
				for i := range pod.Spec.InitContainers {
					if pod.Spec.InitContainers[i].Name == executorTokenWaiter {
						pod.Spec.InitContainers[i] = attacker
					}
				}
				wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "step", Container: &corev1.Container{Image: "pipeline-image"}}}}}
				if shape == "resource" {
					wf.Spec.Templates[0].Container = nil
					wf.Spec.Templates[0].Resource = &wfv1.ResourceTemplate{Action: "get", Manifest: "{}"}
				}
				if err := projectExecutorTokens(wf, controller.config); err != nil {
					t.Fatal(err)
				}
				pod.Spec = applyCredentialWaitPatch(t, pod.Spec, wf.Spec.Templates[0].PodSpecPatch)
				for i := range pod.Spec.InitContainers {
					c := &pod.Spec.InitContainers[i]
					if c.Name != executorTokenWaiter {
						continue
					}
					switch addition {
					case "missing-mount":
						c.VolumeMounts = nil
					case "mount-subPath":
						c.VolumeMounts[0].SubPath = "other"
					case "security":
						c.SecurityContext.Privileged = ptr.To(true)
					case "duplicate":
						pod.Spec.InitContainers = append(pod.Spec.InitContainers, *c.DeepCopy())
					}
					break
				}
				if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, shape == "resource"); err == nil {
					t.Fatal("merged authored fields reached the executor-token waiter")
				}
			})
		}
	}
}

func TestExecutorIdentityExactServerHelpers(t *testing.T) {
	for _, script := range []bool{false, true} {
		for _, modified := range []bool{false, true} {
			t.Run(fmt.Sprintf("script=%v/modified=%v", script, modified), func(t *testing.T) {
				controller, _, pod, _ := executorRetryFixture(t)
				pod.Spec = executorEnvironmentPod(t, "container", "init", nil)
				prepare := nonrootPrepareContainer(script, controller.config.SourceSeedImage)
				prepare.ImagePullPolicy = corev1.PullIfNotPresent
				prepare.TerminationMessagePath = corev1.TerminationMessagePathDefault
				prepare.TerminationMessagePolicy = corev1.TerminationMessageReadFile
				if modified {
					prepare.Env = []corev1.EnvVar{{Name: "PATH", Value: "/loot"}}
				}
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, prepare)
				err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, false)
				if (err != nil) != modified {
					t.Fatalf("server preparation exactness: script=%v modified=%v: %v", script, modified, err)
				}
			})
		}
	}
}
