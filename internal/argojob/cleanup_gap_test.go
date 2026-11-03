package argojob

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type cleanupCancelClient struct{ staticWorkflowClient }

func (*cleanupCancelClient) Delete(_ context.Context, _ string, options metav1.DeleteOptions) error {
	if options.Preconditions == nil || options.Preconditions.UID == nil {
		return errors.New("Workflow delete lacks UID precondition")
	}
	return nil
}

func TestCleanupGapCancelAttemptsBothGuests(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	c, err := NewController(&cleanupCancelClient{staticWorkflowClient{workflow: wf}}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	client := cleanupClient()
	c.WithVMIClient(client)
	for _, suffix := range []string{"amd64", "arm64"} {
		if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), ownedCleanupObject(wf, vmiResource, suffix), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	client.PrependReactor("delete", "virtualmachineinstances", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.HasSuffix(a.(ktesting.DeleteAction).GetName(), "-amd64") {
			return true, nil, apierrors.NewForbidden(vmiResource.GroupResource(), "amd64", errors.New("denied"))
		}
		return false, nil, nil
	})
	observed := false
	c.WithVMICleanupFailure(func(context.Context, string, error) { observed = true })
	if err := c.Cancel(t.Context(), wf.Name, wf.Annotations[runIDAnnotation]); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("cancellation lost cleanup error")
	}
	if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-arm64", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("first error suppressed second guest: %v", err)
	}
}

func TestCleanupGapPendingCreateStillCleansSibling(t *testing.T) {
	config, wf, _ := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	c.kube = cleanupKubeClient()
	client := cleanupClient()
	c.WithVMIClient(client)
	if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), ownedCleanupObject(wf, vmiResource, "amd64"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cm, err := c.getVMILedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ledger := vmiLedger{Version: 1, Claims: map[string]vmiClaim{"pending": {
		WorkflowUID: wf.UID, CleanupWorkflow: wf.Name, CleanupRun: wf.Annotations[runIDAnnotation],
		RepositoryID: 7, PodName: "pending", PodUID: "pending-uid", Name: wf.Name + "-arm64", Hash: strings.Repeat("a", 64), Demand: argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30},
	}}}
	if err := c.updateVMILedger(t.Context(), cm, ledger); err != nil {
		t.Fatal(err)
	}
	completion := Completion{Succeeded: true}
	var log bytes.Buffer
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
	if completion.Succeeded || !strings.Contains(completion.Reason, "unconfirmed VMI create intent") {
		t.Fatalf("in-flight POST absence passed: %+v", completion)
	}
	if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pending sibling suppressed cleanup: %v", err)
	}
	// An API outage beside an identity failure must retain the latter's
	// non-retryable pipeline attribution.
	if cleanupIsInfrastructure(errors.Join(errors.New("foreign owner"), cleanupInfrastructureFailure(context.DeadlineExceeded))) {
		t.Fatal("mixed cleanup failure became retryable")
	}
}

func TestCleanupGapIndependentGuests(t *testing.T) {
	for _, mode := range []string{"delete-forbidden", "foreign-owner", "foreign-label"} {
		t.Run(mode, func(t *testing.T) {
			config, wf, _ := guardedVMIFixture(t, 1)
			wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
			client := cleanupClient()
			first := ownedCleanupObject(wf, vmiResource, "amd64")
			second := ownedCleanupObject(wf, vmiResource, "arm64")
			known := map[string]types.UID{cleanupKey(vmiResource, first.GetName()): first.GetUID()}
			switch mode {
			case "foreign-owner":
				first.SetOwnerReferences(nil)
			case "foreign-label":
				first.SetLabels(nil)
			}
			if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), first, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), second, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
			if err != nil {
				t.Fatal(err)
			}
			c.kube = cleanupKubeClient()
			c.WithVMIClient(client)
			// Inventory must retain safe guests even when a sibling has a
			// bad label or owner.
			objects, readErr := c.cleanupObjects(t.Context(), wf, known, true)
			if mode != "delete-forbidden" && readErr == nil {
				t.Fatal("identity mismatch passed")
			}
			foundSecond := false
			for _, object := range objects {
				foundSecond = foundSecond || object.object.GetName() == second.GetName()
			}
			if !foundSecond {
				t.Fatal("first guest error suppressed second guest inventory")
			}
			if mode == "delete-forbidden" {
				client.PrependReactor("delete", "virtualmachineinstances", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.(ktesting.DeleteAction).GetName() == first.GetName() {
						return true, nil, apierrors.NewForbidden(vmiResource.GroupResource(), first.GetName(), errors.New("denied"))
					}
					return false, nil, nil
				})
			}
			completion := Completion{Succeeded: true}
			var log bytes.Buffer
			c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
			if completion.Succeeded {
				t.Fatal("first guest failure did not gate run")
			}
			if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), second.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("second guest was not deleted: %v", err)
			}
		})
	}
}

func TestCleanupGapAbsenceAndBackingProof(t *testing.T) {
	for _, mode := range []string{"none-created", "one-created", "canceled", "read-forbidden", "read-timeout", "delayed-launcher", "retained-launcher", "retained-backing"} {
		t.Run(mode, func(t *testing.T) {
			config, wf, _ := guardedVMIFixture(t, 1)
			wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
			client := cleanupClient()
			c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
			if err != nil {
				t.Fatal(err)
			}
			kube := cleanupKubeClient()
			c.kube = kube
			c.WithVMIClient(client)
			vmi := ownedCleanupObject(wf, vmiResource, "amd64")
			if mode == "one-created" || mode == "canceled" {
				if _, err := client.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), vmi, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			client.PrependReactor("get", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
				reads++
				if mode == "read-forbidden" {
					return true, nil, apierrors.NewForbidden(vmiResource.GroupResource(), vmi.GetName(), errors.New("denied"))
				}
				if mode == "read-timeout" {
					return true, nil, context.DeadlineExceeded
				}
				return false, nil, nil
			})
			if mode == "delayed-launcher" || mode == "retained-launcher" {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "launcher-random", Namespace: wf.Namespace, UID: "launcher-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: vmi.GetName(), UID: vmi.GetUID()}}}}
				if _, err := kube.CoreV1().Pods(wf.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				polls := 0
				kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					polls++
					if mode == "delayed-launcher" && polls == 3 {
						if err := kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), wf.Namespace, pod.Name); err != nil {
							t.Fatal(err)
						}
					}
					return false, nil, nil
				})
			}
			if mode == "retained-backing" {
				pvc := ownedCleanupObject(wf, pvcResource, "disk")
				if _, err := client.Resource(pvcResource).Namespace(wf.Namespace).Create(t.Context(), pvc, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				client.PrependReactor("delete", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			completion := Completion{Succeeded: true}
			var log bytes.Buffer
			c.cleanupVerdict(ctx, wf, wf.Annotations[runIDAnnotation], &log, &completion)
			wantSuccess := mode == "none-created" || mode == "one-created" || mode == "canceled" || mode == "delayed-launcher"
			if completion.Succeeded != wantSuccess {
				t.Fatalf("completion=%+v log=%s", completion, log.String())
			}
			if reads == 0 {
				t.Fatal("no independent original-name GET")
			}
			if !strings.Contains(log.String(), "run="+wf.Annotations[runIDAnnotation]) || !strings.Contains(log.String(), "uid="+string(wf.UID)) {
				t.Fatal("receipt not bound to run/Workflow")
			}
			if wantSuccess {
				previousReads := reads
				completion = Completion{Succeeded: true}
				c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
				if !completion.Succeeded || reads <= previousReads || completion.Steps[0].Status != runprogress.StepPassed {
					t.Fatal("repeated cleanup did not independently read absence")
				}
			}
		})
	}
}

func TestCleanupGapDurableOriginalUID(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	wf.Annotations[argoworkflow.CleanupVerdictAnnotation] = "1"
	c := quotaController(config, kube, vmi)
	c.workflows = &staticWorkflowClient{workflow: wf}
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Delete(t.Context(), object.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := kube.CoreV1().Pods(wf.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	cm, err := c.getVMILedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := readVMILedger(cm)
	if err != nil {
		t.Fatal(err)
	}
	if !reconcileVMIClaims(&ledger, nil, &corev1.PodList{}, wf.Namespace, time.Now()) {
		t.Fatal("claim did not close")
	}
	if len(ledger.Claims) != 1 {
		t.Fatal("cleanup original UID discarded with resource Pod")
	}
	for _, claim := range ledger.Claims {
		if claim.UID != object.GetUID() || claim.CleanupWorkflow != wf.Name || claim.CleanupRun != wf.Annotations[runIDAnnotation] {
			t.Fatalf("unbound receipt: %+v", claim)
		}
	}
	if err := c.updateVMILedger(t.Context(), cm, ledger); err != nil {
		t.Fatal(err)
	}
	known, err := c.cleanupIdentities(t.Context(), wf)
	if err != nil || known[cleanupKey(vmiResource, object.GetName())] != object.GetUID() {
		t.Fatalf("restart lost original UID: %v %v", known, err)
	}
	vmi = cleanupClient()
	c.WithVMIClient(vmi)
	// A replacement with identical labels AND Workflow owner must survive.
	replacement := object.DeepCopy()
	replacement.SetUID("replacement-uid")
	if _, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	vmi.ClearActions()
	objects, err := c.cleanupObjects(t.Context(), wf, known, true)
	if err == nil || len(objects) != 0 {
		t.Fatalf("replacement accepted: %v %v", objects, err)
	}
	completion := Completion{Succeeded: true}
	var log bytes.Buffer
	c.cleanupVerdict(t.Context(), wf, wf.Annotations[runIDAnnotation], &log, &completion)
	if completion.Succeeded || !strings.Contains(log.String(), "uid="+string(object.GetUID())) {
		t.Fatalf("original/replacement verdict: %+v %s", completion, log.String())
	}
	for _, action := range vmi.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("replacement deleted")
		}
	}
}

func TestCleanupAbsenceGetOuterVerdict(t *testing.T) {
	for _, phase := range []wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed, wfv1.NodeError} {
		t.Run(string(phase), func(t *testing.T) {
			// The pinned Argo executor runs get with --ignore-not-found:
			// an actual 404 yields Succeeded; forbidden/timeout stay errors.
			resource := &wfv1.ResourceTemplate{Action: "get", SetOwnerReference: true, Flags: []string{"--ignore-not-found"}}
			wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf"}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
				{Name: "root", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "verify", Template: "burn"}}}},
				{Name: "burn", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "absence", Template: "read", ContinueOn: &wfv1.ContinueOn{Failed: true, Error: true}}}}},
				{Name: "read", Resource: resource},
			}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowSucceeded, Nodes: wfv1.Nodes{
				"root": {ID: "root", Name: "wf", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
				"burn": {ID: "burn", Name: "wf.verify", DisplayName: "verify", Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeSucceeded},
				"read": {ID: "read", Name: "wf.verify.absence", DisplayName: "absence", TemplateName: "read", Type: wfv1.NodeTypePod, BoundaryID: "burn", Phase: phase},
			}}}
			got := completionFor(wf)
			if len(got.Steps) != 1 || got.Succeeded != (phase == wfv1.NodeSucceeded) {
				t.Fatalf("absence outer completion=%+v", got)
			}
			if phase == wfv1.NodeSucceeded && (got.FailedStep != "" || got.Steps[0].Status != runprogress.StepPassed) {
				t.Fatalf("expected absence marked failed: %+v", got)
			}
		})
	}
}
