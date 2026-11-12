package installer

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/podpriority"
)

func TestPriorityBootstrapIsOwnedIdempotentAndMatchesChart(t *testing.T) {
	cfg := Config{Namespace: "control-a", ArgoNamespace: "pipelines-a"}
	kube := fake.NewClientset()
	deps := Deps{KubeClient: kube}
	if err := ensurePlatformPriorities(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if err := ensurePlatformPriorities(t.Context(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	classes, err := kube.SchedulingV1().PriorityClasses().List(t.Context(), metav1.ListOptions{})
	if err != nil || len(classes.Items) != 2 {
		t.Fatalf("classes: %+v %v", classes, err)
	}
	for _, action := range kube.Actions() {
		if slices.Contains([]string{"patch", "update", "delete"}, action.GetVerb()) {
			t.Fatal("bootstrap mutated an existing class")
		}
	}
	args := []string{"template", "oberth", "../../charts/oberth", "--namespace", cfg.Namespace, "--set", "argo.namespace=" + cfg.ArgoNamespace, "--show-only", "templates/platform-priorityclasses.yaml"}
	out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v %s", err, out)
	}
	for _, doc := range strings.Split(string(out), "\n---") {
		var rendered schedulingv1.PriorityClass
		if err := yaml.Unmarshal([]byte(doc), &rendered); err != nil {
			t.Fatal(err)
		}
		if rendered.Kind != "PriorityClass" {
			continue
		}
		actual, err := kube.SchedulingV1().PriorityClasses().Get(t.Context(), rendered.Name, metav1.GetOptions{})
		if err != nil || actual.Value != rendered.Value || actual.GlobalDefault != rendered.GlobalDefault || actual.PreemptionPolicy == nil || rendered.PreemptionPolicy == nil || *actual.PreemptionPolicy != *rendered.PreemptionPolicy {
			t.Fatalf("bootstrap/chart drift for %s: %v", rendered.Name, err)
		}
		if actual.Annotations["meta.helm.sh/release-name"] != "oberth" || actual.Annotations["meta.helm.sh/release-namespace"] != cfg.Namespace {
			t.Fatal("class cannot be adopted by Oberth Helm release")
		}
	}
	high, _ := kube.SchedulingV1().PriorityClasses().Get(t.Context(), controlPlanePriority(cfg), metav1.GetOptions{})
	low, _ := kube.SchedulingV1().PriorityClasses().Get(t.Context(), podpriority.PipelineClass(cfg.ArgoNamespace), metav1.GetOptions{})
	if high.Value <= low.Value || *high.PreemptionPolicy != corev1.PreemptLowerPriority || *low.PreemptionPolicy != corev1.PreemptNever {
		t.Fatal("control plane cannot preempt non-preempting pipelines")
	}
}

func TestPriorityBootstrapRefusesConflictsAndReadFailures(t *testing.T) {
	for _, kind := range []string{"owner", "value", "preemption", "default", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{}
			kube := fake.NewClientset()
			deps := Deps{KubeClient: kube}
			if err := ensurePlatformPriorities(t.Context(), cfg, deps); err != nil {
				t.Fatal(err)
			}
			class, err := kube.SchedulingV1().PriorityClasses().Get(t.Context(), controlPlanePriority(cfg), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "owner":
				class.Annotations["meta.helm.sh/release-namespace"] = "other"
			case "value":
				class.Value++
			case "preemption":
				policy := corev1.PreemptNever
				class.PreemptionPolicy = &policy
			case "default":
				class.GlobalDefault = true
			case "unreadable":
				kube.PrependReactor("get", "priorityclasses", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, context.DeadlineExceeded })
			}
			if _, err = kube.SchedulingV1().PriorityClasses().Update(t.Context(), class, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := ensurePlatformPriorities(t.Context(), cfg, deps); err == nil {
				t.Fatal("conflicting class accepted")
			}
		})
	}
}

func renderDependencyPods(t *testing.T, cfg Config, chart string, installerArgs []string, kind, expected string) {
	t.Helper()
	args := []string{"template", "dependency", chart}
	for i := 0; i+1 < len(installerArgs); i++ {
		if slices.Contains([]string{"--set", "--set-string", "--set-json", "--values"}, installerArgs[i]) {
			args = append(args, installerArgs[i], installerArgs[i+1])
			i++
		}
	}
	var stderr bytes.Buffer
	command := exec.CommandContext(t.Context(), "helm", args...)
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		t.Fatalf("dependency render: %v %s", err, stderr.String())
	}
	found := false
	for _, doc := range strings.Split(string(out), "\n---") {
		var object struct {
			Kind string
			Spec struct{ Template corev1.PodTemplateSpec }
		}
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil {
			t.Fatal(err)
		}
		if object.Kind == kind {
			found = true
			for _, c := range object.Spec.Template.Spec.Containers {
				if len(c.Resources.Requests) != 0 {
					t.Fatalf("%s retains managed requests: %+v", kind, c.Resources)
				}
			}
			if object.Spec.Template.Spec.PriorityClassName != expected {
				t.Fatalf("%s priority=%s, want %s", kind, object.Spec.Template.Spec.PriorityClassName, expected)
			}
		}
	}
	if !found {
		t.Fatalf("no %s rendered", kind)
	}
}

func TestManagedDependenciesUseUpstreamPodDefaults(t *testing.T) {
	for _, namespace := range []string{"", "control-a"} {
		cfg := Config{Namespace: namespace, InstallSecretStoreDev: true}
		class := ""
		renderDependencyPods(t, cfg, "testdata/argo-workflows-1.0.24.tgz", ArgoHelmArgs(cfg), "Deployment", class)
		renderDependencyPods(t, cfg, "testdata/openbao-0.28.6.tgz", OpenBaoHelmArgs(cfg), "StatefulSet", class)
		cfg.InstallSecretStoreDev = false
		renderDependencyPods(t, cfg, "testdata/openbao-0.28.6.tgz", OpenBaoHelmArgs(cfg), "StatefulSet", class)
	}
}
