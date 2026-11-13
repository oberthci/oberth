package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestExecutionTargetSubmitsExactlyAuthoredPlacement(t *testing.T) {
	for _, site := range []string{"absent", "workflow", "template"} {
		t.Run(site, func(t *testing.T) {
			config, request, objects := placementFixture(t)
			wf, err := argoworkflow.Decode(request.Source)
			if err != nil {
				t.Fatal(err)
			}
			selector := map[string]string{"pool": "different-pool", "kubernetes.io/hostname": "different-node", "kubernetes.io/arch": "arm64"}
			tolerations := []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
			affinity := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "operator.example/rack", Namespaces: []string{"other-namespace"}}}}}
			switch site {
			case "workflow":
				wf.Spec.NodeSelector = selector
				wf.Spec.Tolerations = tolerations
				wf.Spec.Affinity = affinity
			case "template":
				wf.Spec.Templates[0].NodeSelector = selector
				wf.Spec.Templates[0].Tolerations = tolerations
				wf.Spec.Templates[0].Affinity = affinity
			}
			request.Source, err = yaml.Marshal(wf)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := argoworkflow.ResolveRunSource(request.Source, request.Fragments, argoworkflow.RunSourceBinding{RunID: request.RunID, RepositoryID: request.RepositoryID, SHA: request.SHA, File: request.PipelineFile, Trigger: request.effectiveRunTrigger()})
			if err != nil {
				t.Fatal(err)
			}
			p := config.ExecutionPlacement
			p.document.SourceDigest = placementHashRaw(request.Source)
			p.document.GraphDigest = resolved.GraphIdentity()
			p.document.FragmentLockDigest = placementHash(resolved.Lock())
			p.digest = placementHash(p.document)
			request.SourceDir = writeCheckout(t)
			kube := placementClient(objects...)
			kube.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				pod := a.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				pod.UID = "seed-uid"
				pod.ResourceVersion = "1"
				pod.Status.Phase = corev1.PodRunning
				pod.Spec.Tolerations = []corev1.Toleration{{Key: "operator.example/dedicated", Operator: corev1.TolerationOpExists}}
				return false, nil, nil
			})
			kube.PrependReactor("get", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				get := a.(ktesting.GetAction)
				obj, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), get.GetNamespace(), get.GetName())
				if err != nil {
					return false, nil, nil
				}
				pod := obj.(*corev1.Pod).DeepCopy()
				if pod.Labels["oberth.ci/component"] == "source-seed" {
					pod.Spec.NodeName = "operator-chosen-node"
				}
				return true, pod, nil
			})
			client := &createRetryWorkflowClient{}
			controller, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var calls []seedExecCall
			controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&calls), config))
			if _, err = controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if len(client.posts) != 1 {
				t.Fatal("Workflow not submitted", len(client.posts))
			}
			submitted := client.posts[0]
			if submitted.Annotations["oberth.ci/execution-placement"] != p.digest || submitted.Namespace != config.Namespace {
				t.Fatal("approved execution authority lost", submitted.ObjectMeta)
			}
			nativeEqual(t, wf.Spec.NodeSelector, submitted.Spec.NodeSelector)
			nativeEqual(t, wf.Spec.Tolerations, submitted.Spec.Tolerations)
			nativeEqual(t, wf.Spec.Affinity, submitted.Spec.Affinity)
			for i := range wf.Spec.Templates {
				before, after := wf.Spec.Templates[i], submitted.Spec.Templates[i]
				nativeEqual(t, before.NodeSelector, after.NodeSelector)
				nativeEqual(t, before.Tolerations, after.Tolerations)
				nativeEqual(t, before.Affinity, after.Affinity)
			}
			for _, a := range kube.Actions() {
				if a.GetResource().Resource == "nodes" {
					t.Fatal("target placement consulted a node", a)
				}
			}
		})
	}
}
func TestExecutionTargetRetiredPlacementConfigurationRefused(t *testing.T) {
	config, _, _ := placementFixture(t)
	body, err := json.Marshal(config.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"node_name", "node_uid", "provider_id", "zone", "pool", "taint_key", "taint_value"} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		fields[field] = json.RawMessage(`null`)
		raw, _ := json.Marshal(fields)
		if _, err := ParseExecutionPlacement(raw); err == nil || !strings.Contains(err.Error(), "was removed") || !strings.Contains(err.Error(), field) {
			t.Fatal("retired target placement not clearly refused", field, err)
		}
	}
}
