// Package contracttest exercises the injected native and Git schedule ports.
// Spec: docs/plans/c10-schedules.md#inspections
package contracttest

import (
	"context"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/cronsync"
)

type NativeFixture struct {
	Port      cronsync.Native
	Namespace string
}

func RunNative(t *testing.T, create func(*testing.T) NativeFixture) {
	t.Helper()
	t.Run("detached_update_and_preconditions", func(t *testing.T) { nativeLifecycle(t, create(t)) })
	t.Run("cancellation", func(t *testing.T) { nativeCancellation(t, create(t)) })
}
func nativeLifecycle(t *testing.T, f NativeFixture) {
	t.Helper()
	desired := &wfv1.CronWorkflow{ObjectMeta: metav1.ObjectMeta{Name: "contract-parent", Namespace: f.Namespace, Labels: map[string]string{"contract": "owned"}}, Spec: wfv1.CronWorkflowSpec{
		Schedules: []string{"0 3 * * *"}, Suspend: true,
		WorkflowSpec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", Container: &corev1.Container{Image: "image", Command: []string{"true"}}}}},
	}}
	created, err := f.Port.Create(t.Context(), desired, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.UID == "" || created.ResourceVersion == "" {
		t.Fatal("native identity is missing")
	}
	desired.Labels["contract"] = "changed"
	listed, err := f.Port.List(t.Context(), metav1.ListOptions{LabelSelector: "contract=owned"})
	if err != nil || len(listed.Items) != 1 {
		t.Fatal(listed, err)
	}
	nativeUpdate(t, f, created)
}
func nativeUpdate(t *testing.T, f NativeFixture, created *wfv1.CronWorkflow) {
	t.Helper()
	stale := created.DeepCopy()
	created.Labels["phase"] = "updated"
	updated, err := f.Port.Update(t.Context(), created, metav1.UpdateOptions{})
	if err != nil || updated.UID != created.UID {
		t.Fatal(updated, err)
	}
	if _, err := f.Port.Update(t.Context(), stale, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
		t.Fatal("stale update accepted", err)
	}
	updated.Spec.Suspend = false
	got, err := f.Port.Get(t.Context(), created.Name, metav1.GetOptions{})
	if err != nil || !got.Spec.Suspend {
		t.Fatal("returned object aliases storage", err)
	}
	wrong := created.UID + "-replacement"
	if err := f.Port.Delete(t.Context(), created.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &wrong}}); !apierrors.IsConflict(err) {
		t.Fatal("replacement deletion accepted", err)
	}
	if err := f.Port.Delete(t.Context(), created.Name, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationOrphan), Preconditions: &metav1.Preconditions{UID: &created.UID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Port.Get(t.Context(), created.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("deleted object remains", err)
	}
}

func nativeCancellation(t *testing.T, f NativeFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	object := &wfv1.CronWorkflow{ObjectMeta: metav1.ObjectMeta{Name: "cancelled", Namespace: f.Namespace}}
	for _, call := range []func() error{
		func() error { _, err := f.Port.List(ctx, metav1.ListOptions{}); return err },
		func() error { _, err := f.Port.Get(ctx, object.Name, metav1.GetOptions{}); return err },
		func() error { _, err := f.Port.Create(ctx, object, metav1.CreateOptions{}); return err },
		func() error { _, err := f.Port.Update(ctx, object, metav1.UpdateOptions{}); return err },
		func() error { return f.Port.Delete(ctx, object.Name, metav1.DeleteOptions{}) },
	} {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Fatal("native cancellation lost", err)
		}
	}
}
