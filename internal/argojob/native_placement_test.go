package argojob

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type nativePlacement struct {
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	Parallelism  *int64              `json:"parallelism,omitempty"`
}

func nativeEqual(t *testing.T, before, after any) {
	t.Helper()
	a, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("authored fields changed:\n%s\n%s", a, b)
	}
}

// The fixtures are verbatim upstream examples. Ellipsis-only lines in upstream
// explanatory fragments stand for omitted metadata, never scheduling fields.
func nativeExample(t *testing.T, path string) corev1.PodSpec {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(l) != "..." {
			lines = append(lines, l)
		}
	}
	var obj struct {
		Kind string
		Spec struct {
			corev1.PodSpec
			Template struct{ Spec corev1.PodSpec }
		}
	}
	if err = yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Kind == "Deployment" {
		return obj.Spec.Template.Spec
	}
	return obj.Spec.PodSpec
}

func nativeWorkflow(t *testing.T) *wfv1.Workflow {
	t.Helper()
	wf, err := argoworkflow.Decode([]byte(greedyDocument))
	if err != nil {
		t.Fatal(err)
	}
	// Test native fields independently of the server's identity normalization.
	wf.Spec.ServiceAccountName = ""
	wf.Spec.AutomountServiceAccountToken = nil
	return wf
}

func nativeSubmit(t *testing.T, wf *wfv1.Workflow) *wfv1.Workflow {
	t.Helper()
	body, err := yaml.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	config := seedTestConfig()
	kube := fake.NewClientset()
	runningSeedPods(kube)
	client := &createRetryWorkflowClient{}
	controller, err := NewController(client, kube, config)
	if err != nil {
		t.Fatal(err)
	}
	var seeds []seedExecCall
	controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
	request := testRequest(pipeline.TriggerCI, string(body))
	request.SourceDir = writeCheckout(t)
	if _, err = controller.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(client.posts) != 1 {
		t.Fatalf("submitted %d Workflows", len(client.posts))
	}
	return client.posts[0]
}

func TestNativeOfficialPlacementExamplesReachWorkflowAPIUnchanged(t *testing.T) {
	root := "../../pkg/argoworkflow/testdata/native"
	paths, err := filepath.Glob(root + "/placement-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, root+"/node-selector-05.yaml")
	for _, path := range paths {
		pod := nativeExample(t, path)
		for _, site := range []string{"workflow", "template"} {
			t.Run(filepath.Base(path)+"/"+site, func(t *testing.T) {
				wf := nativeWorkflow(t)
				n := int64(137)
				before := nativePlacement{Affinity: pod.Affinity, NodeSelector: pod.NodeSelector, Tolerations: pod.Tolerations, Parallelism: &n}
				if site == "workflow" {
					wf.Spec.Affinity = before.Affinity
					wf.Spec.NodeSelector = before.NodeSelector
					wf.Spec.Tolerations = before.Tolerations
					wf.Spec.Parallelism = before.Parallelism
				} else {
					wf.Spec.Templates[0].Affinity = before.Affinity
					wf.Spec.Templates[0].NodeSelector = before.NodeSelector
					wf.Spec.Templates[0].Tolerations = before.Tolerations
					wf.Spec.Templates[0].Parallelism = before.Parallelism
				}
				submitted := nativeSubmit(t, wf)
				after := nativePlacement{Affinity: submitted.Spec.Affinity, NodeSelector: submitted.Spec.NodeSelector, Tolerations: submitted.Spec.Tolerations, Parallelism: submitted.Spec.Parallelism}
				if site == "template" {
					tmpl := submitted.Spec.Templates[0]
					after = nativePlacement{Affinity: tmpl.Affinity, NodeSelector: tmpl.NodeSelector, Tolerations: tmpl.Tolerations, Parallelism: tmpl.Parallelism}
				}
				nativeEqual(t, before, after)
			})
		}
	}
}

func TestNativeOfficialResourceExamplesReachEveryContainerKindUnchanged(t *testing.T) {
	root := "../../pkg/argoworkflow/testdata/native"
	for _, file := range []string{"resources-01.yaml", "resources-02.yaml", "ephemeral-storage-00.yaml"} {
		pod := nativeExample(t, filepath.Join(root, file))
		for i, container := range pod.Containers {
			for _, kind := range []string{"container", "script", "containerSet", "init", "sidecar"} {
				t.Run(file+"/"+container.Name+"/"+kind, func(t *testing.T) {
					wf := nativeWorkflow(t)
					tmpl := &wf.Spec.Templates[0]
					before := container.Resources.DeepCopy()
					switch kind {
					case "container":
						tmpl.Container.Resources = *before
					case "script":
						c := *tmpl.Container
						tmpl.Container = nil
						c.Resources = *before
						tmpl.Script = &wfv1.ScriptTemplate{Container: c, Source: "true"}
					case "containerSet":
						c := *tmpl.Container
						tmpl.Container = nil
						c.Name = "main"
						c.Resources = *before
						tmpl.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: c}}}
					case "init":
						tmpl.InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "authored-init", Image: tmpl.Container.Image, Command: []string{"true"}, Resources: *before}}}
					case "sidecar":
						tmpl.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "authored-sidecar", Image: tmpl.Container.Image, Command: []string{"true"}, Resources: *before}}}
					}
					submitted := nativeSubmit(t, wf)
					actual := submitted.Spec.Templates[0]
					var after corev1.ResourceRequirements
					switch kind {
					case "container":
						after = actual.Container.Resources
					case "script":
						after = actual.Script.Resources
					case "containerSet":
						after = actual.ContainerSet.Containers[0].Resources
					case "init":
						for _, c := range actual.InitContainers {
							if c.Name == "authored-init" {
								after = c.Resources
							}
						}
					case "sidecar":
						for _, c := range actual.Sidecars {
							if c.Name == "authored-sidecar" {
								after = c.Resources
							}
						}
					}
					nativeEqual(t, before, after)
				})
			}
			_ = i
		}
	}
}

// The reference page describes namespaceSelector but has no YAML example for
// it. These authored combinations supplement its verbatim affinity examples.
func TestNativePlacementPermitsNamespaceAndLabelSelectorsWithoutRestrictions(t *testing.T) {
	wf := nativeWorkflow(t)
	const affinity = `podAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
  - namespaces: [another-namespace]
    namespaceSelector: {}
    labelSelector:
      matchLabels: {any.example/label: anything}
    matchLabelKeys: [pod-template-hash]
    topologyKey: custom.example/rack
podAntiAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
  - namespaceSelector:
      matchLabels: {tenant: other}
    mismatchLabelKeys: [tenant]
    labelSelector:
      matchExpressions: [{key: tenant, operator: Exists}]
    topologyKey: kubernetes.io/hostname
`
	var a corev1.Affinity
	if err := yaml.Unmarshal([]byte(affinity), &a); err != nil {
		t.Fatal(err)
	}
	tolerations := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	wf.Spec.Affinity = &a
	wf.Spec.Tolerations = tolerations
	wf.Spec.Templates[0].Affinity = a.DeepCopy()
	wf.Spec.Templates[0].Tolerations = tolerations
	submitted := nativeSubmit(t, wf)
	nativeEqual(t, wf.Spec.Affinity, submitted.Spec.Affinity)
	nativeEqual(t, wf.Spec.Templates[0].Affinity, submitted.Spec.Templates[0].Affinity)
	nativeEqual(t, tolerations, submitted.Spec.Tolerations)
	nativeEqual(t, tolerations, submitted.Spec.Templates[0].Tolerations)
}
