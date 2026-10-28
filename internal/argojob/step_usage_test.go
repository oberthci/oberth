package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/stepusage"
	"github.com/oberthci/oberth/pkg/periapsis"
)

var measuredUsageDocument = strings.Replace(greedyDocument, "command: [/bin/true]", "command: [/bin/true]\n        resources:\n          limits: {memory: 256Mi}", 1)

// Export the actual admitted command for the opt-in cgroup/Argo regression
// harness. Ordinary go test always exercises the admission assertions below.
const tinyUsageDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: tiny
  activeDeadlineSeconds: 60
  templates:
  - name: tiny
    container:
      image: busybox:1.37@sha256:5cec3fc171c87218698e85a52af7087de727372aae264a787b8112901a5b0092
      command: [/bin/sh, -ec]
      args:
      - |
        data=$(head -c 4194304 /dev/zero | tr '\000' x)
        test "${#data}" -eq 4194304
        test "$(printf 'tiny%s' step)" = tinystep
        printf 'tiny step OK\n'
        for file in /proc/1/status /proc/$PPID/status /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/memory.peak /sys/fs/cgroup/memory.events; do
          printf 'receipt %s\n' "$file"
          while IFS= read -r line; do printf '%s\n' "$line"; done < "$file"
        done
      resources:
        requests: {memory: 16Mi}
        limits: {memory: 32Mi}
`

func TestStepUsageTiny32MiAdmission(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, tinyUsageDocument)
	config := testConfig()
	config.RunnerImagePrefixes = []string{"busybox:"}
	beforeSeed, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	request.SourceVolume = SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", BinarySubPath: binarySubPath}
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	tiny := wf.Spec.Templates[0]
	c := tiny.Container
	if !slices.Equal(c.Command, []string{"/bin/sh", "-ec"}) || c.Resources.Limits.Memory().String() != "32Mi" || c.Resources.Requests.Memory().String() != "16Mi" {
		t.Fatalf("tiny step gained wrapper or changed resources: %+v", c)
	}
	if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == usageBinMountPath }) || strings.Contains(tiny.PodSpecPatch, corev1.LabelArchStable) {
		t.Fatal("unmeasured tiny step gained helper delivery or architecture restriction")
	}
	warnings := stepUsageMemoryWarnings(wf)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `template="tiny"`) || !strings.Contains(warnings[0], "32Mi") || !strings.Contains(warnings[0], "256Mi") || !strings.Contains(warnings[0], "usage remains unknown") {
		t.Fatalf("missing admission warning: %v", warnings)
	}
	if workflowHasStepUsage(wf) || wf.Annotations[usageMemoryPolicyAnnotation] != usageMemoryPolicy {
		t.Fatal("wrong tiny-step policy")
	}
	if !reflect.DeepEqual(stepUsageMemoryWarnings(beforeSeed), warnings) || beforeSeed.Annotations[usageMemoryPolicyAnnotation] != wf.Annotations[usageMemoryPolicyAnnotation] {
		t.Fatal("usage policy depends on per-run source delivery")
	}
	bound, node, pod, _ := diagnosticFixture()
	pod.Spec.Containers[0].Command = c.Command
	pod.Spec.Containers[0].Args = c.Args
	pod.Status.ContainerStatuses[0].State.Terminated.Message = stepusage.Encode(stepusage.Usage{MaxRSSBytes: ptr.To(int64(1234))})
	if got := projectStepUsage(bound, node, pod); got != (stepusage.Usage{}) {
		t.Fatalf("unwrapped step supplied a usage summary: %+v", got)
	}
	if target := os.Getenv("OBERTH_USAGE_MEMORY_EXPORT"); target != "" {
		raw, err := json.Marshal(tiny)
		if err != nil {
			t.Fatal(err)
		}
		// #nosec G703 -- opt-in test harness output, never a server path.
		if err := os.WriteFile(filepath.Clean(target), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStepUsageMemoryFloor(t *testing.T) {
	for _, limit := range []string{"", "0", "32Mi", "64Mi", "128Mi", "255Mi", "268435455", "256Mi", "512Mi"} {
		for _, script := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/script=%v", limit, script), func(t *testing.T) {
				c := corev1.Container{Command: []string{"sh"}, Args: []string{"-e"}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")}}}
				if limit != "" {
					c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}
				}
				before := c.DeepCopy()
				leaf := wfv1.Template{Name: "leaf", Container: &c}
				if script {
					leaf.Container = nil
					leaf.Script = &wfv1.ScriptTemplate{Container: c, Source: "true"}
				}
				// A small inherited default cannot be overlooked when the leaf has
				// no explicit limit. A large explicit limit takes precedence in Argo.
				wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{TemplateDefaults: &wfv1.Template{Container: &corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}}}}, Templates: []wfv1.Template{leaf}}}
				request := Request{SourceVolume: SourceVolume{ClaimName: "source", BinarySubPath: binarySubPath}}
				injectStepUsageMounts(wf, request)
				if err := wrapStepUsage(wf, false); err != nil {
					t.Fatal(err)
				}
				want := limit == "256Mi" || limit == "512Mi"
				if workflowHasStepUsage(wf) != want || (len(stepUsageMemoryWarnings(wf)) == 0) != want {
					t.Fatalf("wrapped=%v warnings=%v", workflowHasStepUsage(wf), stepUsageMemoryWarnings(wf))
				}
				got := usageContainer(&wf.Spec.Templates[0])
				if !reflect.DeepEqual(got.Resources, before.Resources) || !slices.Equal(usageOriginalCommand(got.Command), before.Command) || !slices.Equal(got.Args, before.Args) {
					t.Fatal("memory guard changed resources or original command")
				}
			})
		}
	}
}

func TestStepUsageLowMemoryInlineLeaf(t *testing.T) {
	leaf := &wfv1.Template{Container: &corev1.Container{Command: []string{"sh"}, Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")}}}}
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "parent", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "inline", Inline: leaf}}}}}}}
	injectStepUsageMounts(wf, Request{SourceVolume: SourceVolume{ClaimName: "source", BinarySubPath: binarySubPath}})
	if err := wrapStepUsage(wf, false); err != nil {
		t.Fatal(err)
	}
	if workflowHasStepUsage(wf) || len(stepUsageMemoryWarnings(wf)) != 1 || len(leaf.Container.VolumeMounts) != 0 {
		t.Fatal("inline leaf bypassed the memory guard")
	}
}

func TestStepUsageMemoryPolicyRecovery(t *testing.T) {
	for _, oldWrapper := range []bool{false, true} {
		t.Run(fmt.Sprintf("old-wrapper=%v", oldWrapper), func(t *testing.T) {
			config := testConfig()
			config.RunnerImagePrefixes = []string{"busybox:"}
			request := testRequest(periapsis.TriggerCI, tinyUsageDocument)
			request.SourceVolume = SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath, BinarySubPath: binarySubPath}
			request.legacyStepUsage = oldWrapper // Reconstruct the pre-fix spec.
			existing, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			workflows := &nonrootWorkflowClient{existing: existing}
			controller, err := NewController(workflows, fake.NewClientset(), config)
			if err != nil {
				t.Fatal(err)
			}
			controller.WithSourceSeeder(NewSourceSeeder(controller.kube, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				t.Fatal("recovery must not reseed")
				return nil
			}, config))
			request.SourceVolume = SourceVolume{}
			request.legacyStepUsage = false
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if workflowHasStepUsage(existing) != oldWrapper || workflows.creates != 0 {
				t.Fatal("recovery changed the running spec")
			}
		})
	}
}

func TestStepUsageWrapsExplicitCommandsAfterCredentialAdmission(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, measuredUsageDocument)
	request.SourceVolume = SourceVolume{ClaimName: "source", SubPath: "checkout", BinarySubPath: "oberth-bin"}
	wf, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	measured := 0
	walkTemplates(wf, func(tmpl *wfv1.Template) {
		c := usageContainer(tmpl)
		if c == nil || len(c.Command) == 0 {
			return
		}
		if !slices.Equal(c.Command[:3], []string{usageBinPath, usageCommand, "--"}) {
			t.Fatalf("not wrapped: %+v", c)
		}
		if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.MountPath == usageBinMountPath && m.ReadOnly && m.SubPath == "oberth-bin"
		}) {
			t.Fatal("missing read-only helper")
		}
		if slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == SecretsMountPath }) {
			t.Fatal("measurement gained credentials")
		}
		var patch corev1.PodSpec
		if err := json.Unmarshal([]byte(tmpl.PodSpecPatch), &patch); err != nil || patch.NodeSelector[corev1.LabelArchStable] != runtime.GOARCH || len(tmpl.NodeSelector) != 0 {
			t.Fatalf("helper architecture must be a Pod patch: %s err=%v", tmpl.PodSpecPatch, err)
		}
		measured++
	})
	if measured == 0 {
		t.Fatal("no measured leaf")
	}
	raw, _ := json.Marshal(wf)
	if strings.Contains(string(raw), "hostPath") {
		t.Fatal("usage added hostPath")
	}
	// Scripts retain the interpreter and Argo's later appended script argument.
	script := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "script", Script: &wfv1.ScriptTemplate{Container: corev1.Container{Command: []string{"sh"}, Args: []string{"-e"}}, Source: "echo yes"}}, {Name: "opaque", Container: &corev1.Container{Image: "busybox"}}}}}
	script.Spec.Templates[0].Script.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
	injectStepUsageMounts(script, request)
	script.Spec.Templates[0].NodeSelector = map[string]string{corev1.LabelArchStable: "other", "pool": "build"}
	if err := wrapStepUsage(script, false); err != nil {
		t.Fatal(err)
	}
	if got := script.Spec.Templates[0].NodeSelector; got[corev1.LabelArchStable] != "other" || got["pool"] != "build" || len(script.Spec.Templates[1].NodeSelector) != 0 {
		t.Fatalf("architecture constraint: %+v", script.Spec.Templates)
	}
	if !slices.Equal(script.Spec.Templates[0].Script.Command, []string{usageBinPath, usageCommand, "--", "sh"}) || !slices.Equal(script.Spec.Templates[0].Script.Args, []string{"-e"}) || len(script.Spec.Templates[1].Container.Command) != 0 {
		t.Fatal("changed interpreter/entrypoint")
	}
}

func TestStepUsageFinalAttemptBindingAndMissing(t *testing.T) {
	wf, node, pod, _ := diagnosticFixture()
	main := &pod.Spec.Containers[0]
	main.Command = []string{"/var/run/argo/argoexec", "emissary", "--", usageBinPath, usageCommand, "--", "sh"}
	main.TerminationMessagePath = stepusage.TerminationPath
	expected := stepusage.Usage{MaxRSSBytes: ptr.To(int64(123456)), MemoryPeakBytes: ptr.To(int64(234567)), UserCPU: ptr.To(time.Second), SystemCPU: ptr.To(time.Duration(0))}
	pod.Status.ContainerStatuses[0].State.Terminated.Message = stepusage.Encode(expected)
	controller := &Controller{kube: fake.NewClientset(pod)}
	completion := Completion{Steps: []StepResult{{NodeID: node.ID, PodName: pod.Name}}}
	controller.collectStepDiagnostics(context.Background(), wf, &completion)
	got := completion.Steps[0].Usage
	if got.MaxRSSBytes == nil || *got.MaxRSSBytes != 123456 || got.UserCPU == nil || *got.UserCPU != time.Second || got.SystemCPU == nil || *got.SystemCPU != 0 {
		t.Fatalf("usage %+v", got)
	}
	for _, mutate := range []func(){
		func() { pod.Status.ContainerStatuses[0].RestartCount = 1 },
		func() { pod.Status.ContainerStatuses[0].State.Terminated.Message = "" },
		func() { pod.OwnerReferences[0].UID = "other" },
		func() { main.Command = []string{"sh"} },
	} {
		copyPod := pod.DeepCopy()
		mutate()
		u := projectStepUsage(wf, node, pod)
		if u.MaxRSSBytes != nil || u.UserCPU != nil || u.SystemCPU != nil || u.MemoryPeakBytes != nil {
			t.Fatal("unbound/missing data became measured")
		}
		pod = copyPod
		main = &pod.Spec.Containers[0]
	}
}

func TestStepUsageCreateRecoversLegacyAndMeasuredWithoutReseeding(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		config := testConfig()
		request := testRequest(periapsis.TriggerCI, measuredUsageDocument)
		request.omitStepUsage = legacy
		request.SourceVolume = SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath}
		if !legacy {
			request.SourceVolume.BinarySubPath = binarySubPath
		}
		existing, err := Build(config, request)
		if err != nil {
			t.Fatal(err)
		}
		workflows := &nonrootWorkflowClient{existing: existing}
		controller, err := NewController(workflows, fake.NewClientset(), config)
		if err != nil {
			t.Fatal(err)
		}
		execs := 0
		controller.WithSourceSeeder(NewSourceSeeder(controller.kube, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
			execs++
			return errors.New("must not reseed")
		}, config))
		request.SourceVolume = SourceVolume{}
		request.omitStepUsage = false // Only the server's observed spec selects legacy recovery.
		if _, err := controller.Create(context.Background(), request); err != nil {
			t.Fatalf("legacy=%v recovery: %v", legacy, err)
		}
		if execs != 0 || workflows.creates != 0 {
			t.Fatal("recovery rewrote the source/workflow")
		}
	}
}

func TestStepUsageArchPatchPreservesSchedulingInheritance(t *testing.T) {
	for _, inherited := range []map[string]string{{"pool": "ci"}, {"pool": "parent", "zone": "west"}, {"pool": "ci", corev1.LabelArchStable: "other"}} {
		wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{NodeSelector: inherited, Templates: []wfv1.Template{{Name: "leaf", Container: &corev1.Container{Command: []string{"sh"}, VolumeMounts: []corev1.VolumeMount{{Name: SourceVolumeName, MountPath: usageBinMountPath, ReadOnly: true}}}, PodSpecPatch: `{"nodeSelector":{"disk":"ssd"},"terminationGracePeriodSeconds":7}`}}}}
		wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
		if err := wrapStepUsage(wf, false); err != nil {
			t.Fatal(err)
		}
		leaf := &wf.Spec.Templates[0]
		if len(leaf.NodeSelector) != 0 {
			t.Fatalf("template overrides inherited pool: %v", leaf.NodeSelector)
		}
		before := corev1.PodSpec{NodeSelector: inherited}
		got := applyCredentialWaitPatch(t, before, leaf.PodSpecPatch)
		if got.NodeSelector["pool"] != inherited["pool"] || got.NodeSelector["zone"] != inherited["zone"] || got.NodeSelector["disk"] != "ssd" || got.NodeSelector[corev1.LabelArchStable] != runtime.GOARCH || got.TerminationGracePeriodSeconds == nil || *got.TerminationGracePeriodSeconds != 7 {
			t.Fatalf("scheduling constraints lost: %+v", got)
		}
	}
}
