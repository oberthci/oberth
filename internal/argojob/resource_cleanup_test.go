package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func cleanupClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList", dataVolumeResource: "DataVolumeList", pvcResource: "PersistentVolumeClaimList"})
}

func ownedCleanupObject(wf *wfv1.Workflow, resource schema.GroupVersionResource, suffix string) *unstructured.Unstructured {
	kind := "VirtualMachineInstance"
	if resource == dataVolumeResource {
		kind = "DataVolume"
	}
	if resource == pvcResource {
		kind = "PersistentVolumeClaim"
	}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": resource.GroupVersion().String(), "kind": kind}}
	object.SetName(wf.Name + "-" + suffix)
	object.SetNamespace(wf.Namespace)
	object.SetUID(types.UID(suffix + "-uid"))
	object.SetLabels(map[string]string{"oberth.ci/workflow": wf.Name})
	object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID}})
	return object
}

func TestGuardedResourceDeletes(t *testing.T) {
	for _, resource := range []schema.GroupVersionResource{vmiResource, dataVolumeResource, pvcResource} {
		for _, mode := range []string{"success", "absent", "foreign", "wrong-uid", "replacement", "uid-input", "bad-input", "leftover", "unguarded"} {
			t.Run(resource.Resource+"/"+mode, func(t *testing.T) {
				config, wf, pod := guardedVMIFixture(t, 1)
				object := ownedCleanupObject(wf, resource, "amd64")
				client := cleanupClient()
				if mode == "foreign" {
					object.SetOwnerReferences(nil)
				} // Same run label/name/UID is insufficient.
				if mode != "absent" {
					if _, err := client.Resource(resource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				uid := "amd64-uid"
				if mode == "wrong-uid" {
					uid = "other-uid"
				}
				if mode == "uid-input" || mode == "bad-input" {
					uid = "{{inputs.parameters.uid}}"
					value := "amd64-uid"
					if mode == "bad-input" {
						value = "uid\nmalicious"
					}
					node := wf.Status.Nodes["node"]
					node.Inputs = &wfv1.Inputs{Parameters: []wfv1.Parameter{{Name: "uid", Value: wfv1.AnyStringPtr(value)}}}
					wf.Status.Nodes["node"] = node
				}
				wf.Spec.Templates[0].Resource = &wfv1.ResourceTemplate{Action: "delete", SetOwnerReference: true, Manifest: fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: '{{workflow.name}}-amd64'\n  uid: '%s'\n  labels:\n    oberth.ci/workflow: '{{workflow.name}}'\n", resource.GroupVersion().String(), object.GetKind(), uid)}
				pod.Spec.Containers[0].Command = []string{"argoexec", "version"}
				if mode == "unguarded" {
					pod.Spec.Containers[0].Command = []string{"argoexec", "resource", "delete"}
				}
				if mode == "leftover" {
					client.PrependReactor("delete", resource.Resource, func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
				}
				if mode == "replacement" {
					client.PrependReactor("delete", resource.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
						options := action.(ktesting.DeleteAction).GetDeleteOptions()
						if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != object.GetUID() {
							t.Fatal("delete not UID-pinned")
						}
						return true, nil, apierrors.NewConflict(resource.GroupResource(), object.GetName(), errors.New("UID precondition failed"))
					})
				}
				c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
				if err != nil {
					t.Fatal(err)
				}
				c.kube = cleanupKubeClient()
				c.WithVMIClient(client)
				ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
				defer cancel()
				err = c.ensureVMIForResourcePod(ctx, wf, pod)
				success := mode == "success" || mode == "absent" || mode == "uid-input"
				if success && err != nil {
					t.Fatal(err)
				}
				if mode == "leftover" && !errors.Is(err, errResourceDeletePending) {
					t.Fatalf("slow deletion must remain pending: %v", err)
				}
				if !success && err == nil {
					t.Fatal("unsafe delete passed")
				}
				deletes := 0
				reads := 0
				for _, action := range client.Actions() {
					if action.GetVerb() == "get" {
						reads++
					}
					if action.GetVerb() == "delete" {
						deletes++
						options := action.(ktesting.DeleteAction).GetDeleteOptions()
						if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "amd64-uid" {
							t.Fatal("missing UID precondition")
						}
					}
				}
				if (mode == "foreign" || mode == "wrong-uid" || mode == "bad-input" || mode == "unguarded") && deletes != 0 {
					t.Fatal("refused object received delete")
				}
				if mode == "success" && (deletes != 1 || reads < 2) {
					t.Fatalf("delete=%d read=%d; missing absence readback", deletes, reads)
				}
			})
		}
	}
}

func TestCleanupVerdictGatesWaitAndRecovery(t *testing.T) {
	for _, path := range []string{"wait", "recovery"} {
		for _, mode := range []string{"success", "leftover", "foreign", "unavailable", "incomplete", "already-absent", "cdi-child"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				config, wf, _ := guardedVMIFixture(t, 1)
				wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
				wf.Status.Phase = wfv1.WorkflowSucceeded
				client := cleanupClient()
				for index, resource := range []schema.GroupVersionResource{vmiResource, dataVolumeResource, pvcResource} {
					if mode == "already-absent" {
						continue
					}
					object := ownedCleanupObject(wf, resource, fmt.Sprintf("object%d", index))
					if mode == "foreign" {
						object.SetOwnerReferences(nil)
					}
					if mode == "cdi-child" && resource == pvcResource {
						object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume", Name: wf.Name + "-object1", UID: "object1-uid"}})
						object.SetLabels(nil) // CDI need not copy the Workflow label.
					}
					if _, err := client.Resource(resource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "leftover" {
					client.PrependReactor("delete", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
				}
				if mode == "unavailable" {
					client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewServiceUnavailable("API unavailable")
					})
				}
				if mode == "incomplete" {
					client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, &unstructured.UnstructuredList{Object: map[string]any{"metadata": map[string]any{"continue": "stuck"}}}, nil
					})
				}
				c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
				if err != nil {
					t.Fatal(err)
				}
				c.kube = cleanupKubeClient()
				c.WithVMIClient(client)
				var completion Completion
				var log bytes.Buffer
				if path == "wait" {
					completion, err = c.Wait(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], &log)
				} else {
					var recovered *Completion
					recovered, err = c.TerminalState(t.Context(), wf.Name)
					if recovered != nil {
						completion = *recovered
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				success := mode == "success" || mode == "already-absent" || mode == "cdi-child"
				if completion.Succeeded != success {
					t.Fatalf("completion=%+v", completion)
				}
				if len(completion.Steps) != 1 || completion.Steps[0].Step != "cleanup-verdict" {
					t.Fatalf("missing hook result: %+v", completion.Steps)
				}
				if success && completion.Steps[0].Status != runprogress.StepPassed {
					t.Fatal("hook did not pass")
				}
				if !success && (completion.FailedStep != "cleanup-verdict" || completion.Steps[0].Status != runprogress.StepFailed || !strings.Contains(completion.Reason, "FAIL")) {
					t.Fatalf("failed hook did not gate: %+v", completion)
				}
				if path == "wait" && !strings.Contains(log.String(), "[oberth-cleanup/cleanup-verdict]") {
					t.Fatalf("missing step log: %s", log.String())
				}
				if mode == "leftover" {
					for index := 0; index < 3; index++ {
						name := fmt.Sprintf("object%d", index)
						if !strings.Contains(completion.Reason, name) || (path == "wait" && !strings.Contains(log.String(), name)) {
							t.Fatalf("missing leftover %s in log/verdict", name)
						}
					}
				}
				for _, action := range client.Actions() {
					if action.GetVerb() == "delete" {
						if mode == "foreign" {
							t.Fatal("foreign object was deleted")
						}
						options := action.(ktesting.DeleteAction).GetDeleteOptions()
						if options.Preconditions == nil || options.Preconditions.UID == nil {
							t.Fatal("hook delete missing UID")
						}
					}
				}
			})
		}
	}
}

func TestBuildProjectsDeleteIntoServerGuard(t *testing.T) {
	source := strings.Replace(kubeVirtWorkflow, "action: create", "action: delete", 1)
	begin := strings.Index(source, "          spec:\n")
	source = source[:begin]
	_, wf, _ := guardedVMISourceFixture(t, 1, 7, source)
	if wf.Spec.Templates[0].Resource.Action != "delete" || !strings.Contains(wf.Spec.Templates[0].PodSpecPatch, `"command":["argoexec","version"]`) {
		t.Fatalf("delete bypasses server guard: %+v", wf.Spec.Templates[0])
	}
}

func TestCleanupVerdictRefusesUnlabeledForeignOrOrphan(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	client := cleanupClient()
	object := ownedCleanupObject(wf, pvcResource, "orphan")
	object.SetOwnerReferences(nil)
	object.SetLabels(nil)
	if _, err := client.Resource(pvcResource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	c.WithVMIClient(client)
	completion := Completion{Succeeded: true}
	var log bytes.Buffer
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
	if completion.Succeeded || !strings.Contains(log.String(), object.GetName()) {
		t.Fatalf("unproven absence passed: %+v %s", completion, log.String())
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("foreign/orphan object deleted")
		}
	}
}

type brokenCleanupLog struct{}

func (brokenCleanupLog) Write([]byte) (int, error) { return 0, errors.New("log storage unavailable") }

func TestCleanupVerdictRequiresRetainedStepLog(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	c.WithVMIClient(cleanupClient())
	completion := Completion{Succeeded: true}
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], brokenCleanupLog{}, &completion)
	if completion.Succeeded || !strings.Contains(completion.Reason, "log storage unavailable") {
		t.Fatalf("passed without step log: %+v", completion)
	}
}

func TestCleanupVerdictPreservesPipelineFailure(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	c.WithVMIClient(cleanupClient())
	completion := Completion{Succeeded: false, FailedBurn: "test", FailedStep: "driver", Reason: "guest failed"}
	var log bytes.Buffer
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
	if completion.Succeeded || completion.FailedStep != "driver" || completion.Reason != "guest failed" || completion.Steps[0].Status != runprogress.StepPassed {
		t.Fatalf("pipeline failure changed: %+v", completion)
	}
}

func TestCleanupPollBackoffCapsAtTwoSeconds(t *testing.T) {
	delay := initialCleanupPollDelay
	for _, want := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 2 * time.Second, 2 * time.Second} {
		if delay != want {
			t.Fatalf("poll delay %s, want %s", delay, want)
		}
		delay = nextCleanupPollDelay(delay)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitCleanupPoll(ctx, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("backoff ignored deadline/cancellation: %v", err)
	}
}

func TestCleanupVerdictBackoffBoundsListRate(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	client := cleanupClient()
	object := ownedCleanupObject(wf, pvcResource, "slow")
	object.SetLabels(nil)
	if _, err := client.Resource(pvcResource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("delete", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	c.WithVMIClient(client)
	completion := Completion{Succeeded: true}
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], nil, &completion)
	counts := map[string]int{}
	fallback := map[string]int{}
	reads := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" {
			if action.(ktesting.ListAction).GetListRestrictions().Labels.String() != "" {
				counts[action.GetResource().Resource]++
			} else {
				fallback[action.GetResource().Resource]++
			}
		}
		if action.GetVerb() == "get" {
			reads++
		}
	}
	for resource, count := range counts {
		if count > 4 || count < 2 {
			t.Fatalf("%s: %d LISTs in one-second verdict, want 2..4 backoff polls", resource, count)
		}
	}
	for resource, count := range fallback {
		if count != 1 {
			t.Fatalf("%s repeated shared-namespace scans while pending: %d", resource, count)
		}
	}
	if reads < 2 {
		t.Fatal("unlabeled pending object was not observed by name")
	}
	if completion.Succeeded || !strings.Contains(completion.Reason, object.GetName()) {
		t.Fatalf("backoff lost bounded leftover verdict: %+v", completion)
	}
}

func TestCleanupVerdictSharedNamespaceInventory(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "5"
	client := cleanupClient()
	for index := range 2050 {
		object := ownedCleanupObject(wf, pvcResource, fmt.Sprintf("unrelated%d", index))
		object.SetName(fmt.Sprintf("foreign-%04d", index))
		object.SetLabels(nil)
		object.SetOwnerReferences(nil)
		if _, err := client.Resource(pvcResource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	object := ownedCleanupObject(wf, pvcResource, "unlabeled")
	object.SetLabels(nil)
	if _, err := client.Resource(pvcResource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The real API paginates namespace fallback scans; unrelated pages must
	// neither consume the run bound nor hide an unlabeled run PVC on page 9.
	scoped, fallbackPages := 0, 0
	client.PrependReactor("list", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		listAction := action.(ktesting.ListAction)
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		raw, err := client.Tracker().List(pvcResource, schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, wf.Namespace)
		if err != nil {
			return true, nil, err
		}
		list := raw.(*unstructured.UnstructuredList)
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].GetName() < list.Items[j].GetName() })
		filtered := []unstructured.Unstructured{}
		for _, item := range list.Items {
			if listAction.GetListRestrictions().Labels.Matches(labels.Set(item.GetLabels())) {
				filtered = append(filtered, item)
			}
		}
		start := 0
		if options.Continue != "" {
			start, err = strconv.Atoi(options.Continue)
			if err != nil {
				return true, nil, err
			}
		}
		end := min(start+int(options.Limit), len(filtered))
		page := &unstructured.UnstructuredList{Items: filtered[start:end]}
		if end < len(filtered) {
			page.SetContinue(strconv.Itoa(end))
		}
		if options.LabelSelector == "" {
			fallbackPages++
		} else {
			scoped++
		}
		return true, page, nil
	})
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	c.WithVMIClient(client)
	completion := Completion{Succeeded: true}
	var log bytes.Buffer
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
	if !completion.Succeeded {
		t.Fatalf("unrelated PVCs broke cleanup: %+v %s", completion, log.String())
	}
	if scoped == 0 || fallbackPages < 18 {
		t.Fatalf("missing scoped inventory or complete initial/final fallback: scoped=%d pages=%d", scoped, fallbackPages)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.(ktesting.DeleteAction).GetName() != object.GetName() {
			t.Fatal("deleted an unrelated PVC")
		}
	}
	remaining, err := client.Resource(pvcResource).Namespace(wf.Namespace).List(t.Context(), metav1.ListOptions{Limit: 256})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items) != 256 || remaining.GetContinue() == "" {
		t.Fatal("unrelated namespace PVCs did not survive")
	}
}

func TestCleanupVerdictFinalPrefixScanFindsLateUnlabeledPVC(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "2"
	client := cleanupClient()
	vmi := ownedCleanupObject(wf, vmiResource, "vm")
	pvc := ownedCleanupObject(wf, pvcResource, "late")
	pvc.SetLabels(nil)
	if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), vmi, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("delete", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
		if err := client.Tracker().Create(pvcResource, pvc, wf.Namespace); err != nil {
			t.Fatal(err)
		}
		return false, nil, nil
	})
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	c.WithVMIClient(client)
	completion := Completion{Succeeded: true}
	var log bytes.Buffer
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
	if !completion.Succeeded || !strings.Contains(log.String(), pvc.GetName()) {
		t.Fatalf("late PVC escaped cleanup: %+v %s", completion, log.String())
	}
	if _, err := client.Resource(pvcResource).Namespace(wf.Namespace).Get(t.Context(), pvc.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("late PVC survived: %v", err)
	}
}

func TestCleanupVerdictFailureAttribution(t *testing.T) {
	for _, mode := range []string{"nil-client", "list-forbidden", "delete-forbidden", "api-unavailable", "log-writer", "leftover", "foreign", "leftover-and-log"} {
		for _, pipelineFailed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pipeline-failed=%t", mode, pipelineFailed), func(t *testing.T) {
				config, wf, _ := guardedVMIFixture(t, 1)
				wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
				client := cleanupClient()
				if mode == "leftover" || mode == "leftover-and-log" || mode == "delete-forbidden" || mode == "foreign" {
					object := ownedCleanupObject(wf, vmiResource, "leftover")
					if mode == "foreign" {
						object.SetOwnerReferences(nil)
					}
					if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "list-forbidden" {
					client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(pvcResource.GroupResource(), "", errors.New("RBAC unavailable"))
					})
				}
				if mode == "api-unavailable" {
					client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewServiceUnavailable("API unavailable")
					})
				}
				if mode == "delete-forbidden" {
					client.PrependReactor("delete", "*", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(vmiResource.GroupResource(), "leftover", errors.New("RBAC unavailable"))
					})
				}
				if mode == "leftover" || mode == "leftover-and-log" {
					client.PrependReactor("delete", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
				}
				c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
				if err != nil {
					t.Fatal(err)
				}
				c.kube = cleanupKubeClient()
				if mode != "nil-client" {
					c.kube = cleanupKubeClient()
					c.WithVMIClient(client)
				}
				completion := Completion{Succeeded: !pipelineFailed}
				if pipelineFailed {
					completion.FailedBurn = "test"
					completion.FailedStep = "driver"
					completion.Reason = "guest failed"
				}
				var log bytes.Buffer
				var destination io.Writer = &log
				if mode == "log-writer" || mode == "leftover-and-log" {
					destination = brokenCleanupLog{}
				}
				c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], destination, &completion)
				wantInfra := !pipelineFailed && mode != "leftover" && mode != "foreign" && mode != "leftover-and-log"
				if completion.Succeeded || completion.Infrastructure != wantInfra {
					t.Fatalf("wrong cleanup attribution: %+v", completion)
				}
				if pipelineFailed && (completion.FailedStep != "driver" || !strings.Contains(completion.Reason, "guest failed")) {
					t.Fatalf("pipeline failure masked: %+v", completion)
				}
			})
		}
	}
}

func cleanupKubeClient() *fake.Clientset {
	return fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: vmiQuotaConfigMap, Namespace: testNamespace, Labels: map[string]string{"oberth.ci/role": "vmi-reservations"}, Annotations: map[string]string{vmiLedgerInitialized: "1"}}, Data: map[string]string{"reservations.json": `{"version":1,"claims":{}}`}})
}
