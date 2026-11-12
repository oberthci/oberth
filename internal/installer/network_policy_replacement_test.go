package installer

import (
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestReconciliationPreservesReplacementNamespace(t *testing.T) {
	token := "0123456789abcdef"
	stale := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth-policy-probe-" + token, UID: types.UID("expired-uid"), Labels: map[string]string{policyProbeLabel: token}, Annotations: map[string]string{policyProbeExpiry: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}}}
	replacement := stale.DeepCopy()
	replacement.UID = types.UID("new-active-uid")
	replacement.Annotations[policyProbeExpiry] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	kube := fake.NewClientset(stale)
	replaced := false
	kube.PrependReactor("get", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !replaced {
			replaced = true
			resource := corev1.SchemeGroupVersion.WithResource("namespaces")
			if err := kube.Tracker().Delete(resource, "", stale.Name); err != nil {
				t.Fatal(err)
			}
			if err := kube.Tracker().Create(resource, replacement, ""); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil, nil
	})
	deleted := ""
	kube.PrependReactor("delete", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		options := a.(k8stesting.DeleteAction).GetDeleteOptions()
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			deleted = string(*options.Preconditions.UID)
		}
		return false, nil, nil
	})
	_, err := reconcilePolicyProbes(t.Context(), Deps{KubeClient: kube}, policyProbeTiming{interval: time.Millisecond})
	t.Logf("reconcileError=%v deletedUID=%q", err, deleted)
	if err == nil {
		t.Fatal("replacement was not reported")
	}
	if _, getErr := kube.CoreV1().Namespaces().Get(t.Context(), replacement.Name, metav1.GetOptions{}); getErr != nil {
		t.Fatal("replacement disappeared", getErr)
	}
	if deleted == string(replacement.UID) {
		t.Fatal("stale list observation authorized deletion of a replacement namespace with a new UID and future expiry")
	}
}

func TestReconciliationRequiresKnownLifetimeAndStableVersion(t *testing.T) {
	for _, mode := range []string{"missing expiry", "malformed expiry", "renewed", "replaced before delete"} {
		t.Run(mode, func(t *testing.T) {
			token := "0123456789abcdef"
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth-policy-probe-" + token, UID: "listed-uid", ResourceVersion: "1", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)), Labels: map[string]string{policyProbeLabel: token}, Annotations: map[string]string{policyProbeExpiry: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}}}
			if mode == "missing expiry" {
				delete(ns.Annotations, policyProbeExpiry)
			}
			if mode == "malformed expiry" {
				ns.Annotations[policyProbeExpiry] = "invalid"
			}
			kube := fake.NewClientset(ns)
			kube.PrependReactor("get", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if mode == "renewed" {
					renewed := ns.DeepCopy()
					renewed.ResourceVersion = "2"
					renewed.Annotations[policyProbeExpiry] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
					return true, renewed, nil
				}
				return false, nil, nil
			})
			deletes := 0
			kube.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				deletes++
				opts := action.(k8stesting.DeleteAction).GetDeleteOptions()
				if mode != "replaced before delete" || opts.Preconditions == nil || *opts.Preconditions.UID != ns.UID || opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "1" {
					t.Fatal("stale cleanup authority changed", opts)
				}
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "namespaces"}, ns.Name, errors.New("replacement won"))
			})
			evidence, err := reconcilePolicyProbes(t.Context(), Deps{KubeClient: kube}, policyProbeTiming{interval: time.Millisecond})
			if err == nil || evidence == "" {
				t.Fatal("changed/unknown lifetime not reported", evidence, err)
			}
			if mode != "replaced before delete" && deletes != 0 {
				t.Fatal("unauthorized deletion", mode)
			}
			if _, err := kube.CoreV1().Namespaces().Get(t.Context(), ns.Name, metav1.GetOptions{}); err != nil {
				t.Fatal("namespace was deleted", err)
			}
		})
	}
}
