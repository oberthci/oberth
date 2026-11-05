package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestExecutionTargetExpiredCustodyRecovery(t *testing.T) {
	for _, owner := range []string{"running", "restart-original", "restart-renewed"} {
		t.Run(owner, func(t *testing.T) {
			f := newTargetRouteFixture(t)
			ctx := t.Context()
			body, err := json.Marshal(f.definition.Config.ExecutionPlacement)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(5 * time.Second)
			fields["expires"], _ = json.Marshal(expires)
			body, _ = json.Marshal(fields)
			f.definition.Config.ExecutionPlacement, err = argojob.ParseExecutionPlacement(body)
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := WithExecutionTargets(f.local, f.db, []ExecutionTarget{f.definition})
			if err != nil {
				t.Fatal(err)
			}
			request := f.request
			if _, err := jobs.PipelineSize(ctx, request); err != nil {
				t.Fatal(err)
			}
			group, limit, err := jobs.PipelineConcurrencyContract(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetRunConcurrencyGroup(ctx, request.Run.ID, group, limit); err != nil {
				t.Fatal(err)
			}
			request.Run.ConcurrencyGroup, request.Run.ConcurrencyLimit = group, limit
			request.ConcurrencyGroup = group
			request.DemandReceipt, err = jobs.PipelineDemandReceipt(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := jobs.AwaitRunAdmission(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := jobs.CreateWorkflow(ctx, request); err != nil {
				t.Fatal(err)
			}
			binding, found, err := f.db.ExecutionTarget(ctx, request.Run.ID, request.WorkflowName)
			if err != nil || !found {
				t.Fatal(binding, found, err)
			}
			rows, err := f.db.ExecutionTargetResources(ctx, request.Run.ID)
			if err != nil || len(rows) != 3 || f.remote.posts != 1 {
				t.Fatal(rows, err, f.remote.posts)
			}
			// Simulate a still-present original seed after source delivery. Cleanup
			// must dispose the original Pod and PVC as well as the Workflow.
			for _, row := range rows {
				if row.UID == "" {
					t.Fatal("original UID not durable", row)
				}
				if row.Kind != "Pod" {
					continue
				}
				var pod corev1.Pod
				if err := json.Unmarshal(row.Object, &pod); err != nil {
					t.Fatal(err)
				}
				pod.UID = "remote-seed-uid"
				pod.ResourceVersion = "1"
				if err := f.kube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), &pod, row.Namespace); err != nil {
					t.Fatal(err)
				}
			}
			// The Kubernetes fake does not implement UID preconditions itself.
			// Enforce them against actual objects so replacement deletion cannot pass.
			f.kube.PrependReactor("delete", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				deleteAction := action.(ktesting.DeleteAction)
				object, err := f.kube.Tracker().Get(action.GetResource(), action.GetNamespace(), deleteAction.GetName())
				if err != nil {
					return true, nil, err
				}
				options := deleteAction.GetDeleteOptions()
				actual := object.(metav1.Object)
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != actual.GetUID() {
					return true, nil, errors.New("original UID precondition refused replacement")
				}
				return false, nil, nil
			})
			if !time.Now().Before(expires) {
				t.Fatal("fixture setup exceeded expiry window")
			}
			timer := time.NewTimer(time.Until(expires) + 20*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err := argojob.ParseExecutionPlacement(body); err == nil {
				t.Fatal("expired policy admitted")
			}
			if owner == "restart-renewed" {
				fields["expires"], _ = json.Marshal(time.Now().Add(time.Hour))
				current, _ := json.Marshal(fields)
				f.definition.Config.ExecutionPlacement, err = argojob.ParseExecutionPlacement(current)
				if err != nil {
					t.Fatal(err)
				}
				// A later policy must not replace the original executor configuration.
				f.definition.Config.WorkflowTimeout = time.Hour
			}
			if owner != "running" {
				jobs, err = WithExecutionTargets(f.local, f.db, []ExecutionTarget{f.definition})
				if err != nil {
					t.Fatal("expired original target must remain recoverable", err)
				}
			}
			if namespace, err := jobs.WorkflowNamespaceForRun(ctx, request.Run); err != nil || namespace != binding.Namespace {
				t.Fatal(namespace, err)
			}
			if owner != "running" {
				if size, found, err := jobs.Adopt(ctx, request.Run); err != nil || !found || size != pipeline.L {
					t.Fatal("expired recovery", size, found, err)
				}
			}
			if owns, err := jobs.Owns(ctx, request.WorkflowName); err != nil || !owns {
				t.Fatal(owns, err)
			}
			if observation, err := jobs.ObserveScheduling(ctx, request.Run); err != nil || observation == nil {
				t.Fatal(observation, err)
			}
			if err := jobs.CreateWorkflow(ctx, request); err == nil {
				t.Fatal("expired binding allowed another execution")
			}
			child, err := jobs.routes.forName(ctx, request.WorkflowName, request.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := child.targetClients.Kubernetes().CoreV1().Pods(binding.Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: request.WorkflowName + "-src-collect", Namespace: binding.Namespace}}, metav1.CreateOptions{}); err == nil {
				t.Fatal("expired custody allowed allocation")
			}
			if owner == "running" {
				f.remote.err = errors.New("fixture read denied")
				if err := jobs.Delete(ctx, request.WorkflowName, request.Run.ID); err == nil || f.remote.deletes != 0 {
					t.Fatal("denied read released custody", err)
				}
				f.remote.err = nil
				if cancelled, err := f.db.ExecutionTargetCancelled(ctx, request.Run.ID); err != nil || !cancelled {
					t.Fatal(cancelled, err)
				}
				original := f.remote.object.DeepCopy()
				f.remote.object.UID = "replacement-workflow"
				if err := jobs.Delete(ctx, request.WorkflowName, request.Run.ID); err == nil || f.remote.deletes != 0 || f.remote.object.UID != "replacement-workflow" {
					t.Fatal("expired cleanup deleted a replacement Workflow", err)
				}
				f.remote.object = original
			}
			// The POST horizon still retains canonical custody after deletion.
			if err := jobs.Delete(ctx, request.WorkflowName, request.Run.ID); !errors.Is(err, argojob.ErrTargetUncertain) {
				t.Fatal("cleanup horizon lost", err)
			}
			if f.remote.object != nil || f.remote.deletes != 1 {
				t.Fatal("original Workflow survived cleanup", f.remote.deletes)
			}
			for _, row := range rows {
				var resource string
				switch row.Kind {
				case "Pod":
					resource = "pods"
				case "PersistentVolumeClaim":
					resource = "persistentvolumeclaims"
				default:
					continue
				}
				_, err := f.kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource(resource), row.Namespace, row.Name)
				if !apierrors.IsNotFound(err) {
					t.Fatal("original resource survived cleanup", row.Kind, err)
				}
			}
			after, found, err := f.db.ExecutionTarget(ctx, request.Run.ID, request.WorkflowName)
			if err != nil || !found || !reflect.DeepEqual(binding, after) {
				t.Fatal("retained authority changed", err)
			}
			if cancelled, err := f.db.ExecutionTargetCancelled(ctx, request.Run.ID); err != nil || !cancelled {
				t.Fatal(cancelled, err)
			}
			if retained, err := f.db.ExecutionTargetResources(ctx, request.Run.ID); err != nil || !reflect.DeepEqual(rows, retained) {
				t.Fatal("original UIDs or custody horizon changed", err)
			}
			if f.remote.posts != 1 {
				t.Fatal("duplicate Workflow allocation", f.remote.posts)
			}
			f.noLocalEffects(t)
		})
	}
}
