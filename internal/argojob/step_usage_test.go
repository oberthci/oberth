package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/stepusage"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestStepUsageWrapsExplicitCommandsAfterCredentialAdmission(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, greedyDocument)
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
		if tmpl.NodeSelector[corev1.LabelArchStable] != runtime.GOARCH {
			t.Fatalf("helper architecture missing: %v", tmpl.NodeSelector)
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
	injectStepUsageMounts(script, request)
	script.Spec.Templates[0].NodeSelector = map[string]string{corev1.LabelArchStable: "other", "pool": "build"}
	wrapStepUsage(script)
	if got := script.Spec.Templates[0].NodeSelector; got[corev1.LabelArchStable] != runtime.GOARCH || got["pool"] != "build" || len(script.Spec.Templates[1].NodeSelector) != 0 {
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
		request := testRequest(periapsis.TriggerCI, greedyDocument)
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
