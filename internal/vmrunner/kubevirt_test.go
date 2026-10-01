package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func transportConfig() KubeVirtConfig {
	return KubeVirtConfig{Namespace: "pipeline", ServerNamespace: "server", GuestServiceAccount: "vm-guest", RequestTimeout: time.Second, PollInterval: time.Millisecond}
}

func transportFixture(t *testing.T) (*KubeVirtBackend, *fake.FakeDynamicClient) {
	t.Helper()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList", podResource: "PodList"})
	client.PrependReactor("create", "virtualmachineinstances", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("owned-vm-uid")
		object.SetCreationTimestamp(metav1.NewTime(time.Now().UTC().Truncate(time.Second)))
		return false, nil, nil
	})
	backend, err := NewKubeVirtBackend(client, transportConfig())
	if err != nil {
		t.Fatal(err)
	}
	return backend, client
}

func createTransportVMI(t *testing.T, backend *KubeVirtBackend) VMInstance {
	t.Helper()
	instance, err := backend.CreateVMI(context.Background(), "pipeline", validSpec())
	if err != nil {
		t.Fatal(err)
	}
	if instance.UID == "" || instance.SpecIdentity != SpecIdentity(validSpec()) || instance.CreatedAt.IsZero() {
		t.Fatalf("incomplete receipt: %#v", instance)
	}
	return instance
}

func TestKubeVirtClosedGuestSpec(t *testing.T) {
	backend, client := transportFixture(t)
	instance := createTransportVMI(t, backend)
	object, err := client.Resource(vmiResource).Namespace("pipeline").Get(context.Background(), instance.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		path  []string
		value any
	}{
		{[]string{"spec", "serviceAccountName"}, "vm-guest"},
		{[]string{"spec", "domain", "devices", "autoattachPodInterface"}, false},
		{[]string{"spec", "domain", "devices", "autoattachVSOCK"}, false},
		{[]string{"spec", "domain", "devices", "disableHotplug"}, true},
		{[]string{"spec", "domain", "firmware", "kernelBoot", "container", "image"}, validSpec().GuestImageRef},
	}
	for _, check := range checks {
		value, found, err := unstructured.NestedFieldNoCopy(object.Object, check.path...)
		if err != nil || !found || value != check.value {
			t.Fatalf("guest boundary %v = %v (%v)", check.path, value, err)
		}
	}
	networks, _, _ := unstructured.NestedSlice(object.Object, "spec", "networks")
	if len(networks) != 0 {
		t.Fatal("offline guest received a network")
	}
	body, _ := json.Marshal(object.Object["spec"])
	for _, denied := range []string{"hostPath", "secret", "cloudInit", "accessCredentials", "serviceAccountToken", "hostNetwork"} {
		if strings.Contains(string(body), denied) {
			t.Fatalf("guest spec includes %s", denied)
		}
	}
	// An idempotent retry observes the same exact object, not a new execution.
	retry := createTransportVMI(t, backend)
	if retry != instance {
		t.Fatalf("create retry changed identity: %#v", retry)
	}
}

func TestKubeVirtRejectsInjectedSpecDespiteUnchangedHash(t *testing.T) {
	for _, attack := range []string{"network", "secret", "image", "cpu", "hook", "unknown", "admission"} {
		t.Run(attack, func(t *testing.T) {
			backend, client := transportFixture(t)
			instance := createTransportVMI(t, backend)
			object, err := client.Resource(vmiResource).Namespace("pipeline").Get(context.Background(), instance.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch attack {
			case "network":
				err = unstructured.SetNestedField(object.Object, true, "spec", "domain", "devices", "autoattachPodInterface")
			case "secret":
				err = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "credentials", "secret": map[string]any{"secretName": "token"}}}, "spec", "volumes")
			case "image":
				err = unstructured.SetNestedField(object.Object, "evil/image:latest", "spec", "domain", "firmware", "kernelBoot", "container", "image")
			case "cpu":
				err = unstructured.SetNestedField(object.Object, int64(64), "spec", "domain", "cpu", "cores")
			case "hook":
				annotations := object.GetAnnotations()
				annotations["hooks.kubevirt.io/hookSidecars"] = "[]"
				object.SetAnnotations(annotations)
			case "unknown":
				err = unstructured.SetNestedField(object.Object, true, "spec", "futurePrivilege")
			case "admission":
				annotations := object.GetAnnotations()
				annotations[vmSpecAnnotation] += "{}"
				object.SetAnnotations(annotations)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Tracker().Update(vmiResource, object, "pipeline"); err != nil {
				t.Fatal(err)
			}
			if _, err := backend.GetVMI(context.Background(), "pipeline", instance.Name); err == nil {
				t.Fatalf("accepted %s injection with unchanged intent hash", attack)
			}
		})
	}
}

func TestKubeVirtDeletionRefusesReplacementAndUsesUIDPrecondition(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			backend, client := transportFixture(t)
			instance := createTransportVMI(t, backend)
			if replace {
				object, _ := client.Resource(vmiResource).Namespace("pipeline").Get(context.Background(), instance.Name, metav1.GetOptions{})
				object.SetUID("replacement")
				if err := client.Tracker().Update(vmiResource, object, "pipeline"); err != nil {
					t.Fatal(err)
				}
			}
			deletions := 0
			client.PrependReactor("delete", "virtualmachineinstances", func(action ktesting.Action) (bool, runtime.Object, error) {
				deletions++
				options := action.(ktesting.DeleteAction).GetDeleteOptions()
				if options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != instance.UID || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
					t.Fatal("delete omitted exact UID/foreground constraints")
				}
				return false, nil, nil
			})
			err := backend.DeleteVMI(context.Background(), "pipeline", instance)
			if replace {
				if err == nil || deletions != 0 {
					t.Fatalf("replacement deleted: %v (%d requests)", err, deletions)
				}
			} else if err != nil || deletions != 1 {
				t.Fatalf("owned cleanup: %v (%d requests)", err, deletions)
			}
		})
	}
}

func TestKubeVirtCleanupWaitsForAbsence(t *testing.T) {
	backend, client := transportFixture(t)
	instance := createTransportVMI(t, backend)
	client.PrependReactor("delete", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := backend.DeleteVMI(ctx, "pipeline", instance); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acknowledged delete counted as cleanup: %v", err)
	}
}

func TestKubeVirtCleanupFindsSurvivingOwnedLauncher(t *testing.T) {
	backend, client := transportFixture(t)
	instance := createTransportVMI(t, backend)
	if err := client.Tracker().Delete(vmiResource, "pipeline", instance.Name); err != nil {
		t.Fatal(err)
	}
	for _, own := range []bool{true, false} {
		uid := instance.UID
		if !own {
			uid = "other-vmi"
		}
		controller := true
		pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": fmt.Sprint("launcher-", own), "namespace": "pipeline", "uid": fmt.Sprint("pod-", own)}}}
		pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: instance.Name, UID: types.UID(uid), Controller: &controller}})
		if err := client.Tracker().Create(podResource, pod, "pipeline"); err != nil {
			t.Fatal(err)
		}
	}
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		deleteAction := action.(ktesting.DeleteAction)
		if deleteAction.GetName() != "launcher-true" || deleteAction.GetDeleteOptions().Preconditions == nil || deleteAction.GetDeleteOptions().Preconditions.UID == nil || *deleteAction.GetDeleteOptions().Preconditions.UID != "pod-true" {
			t.Fatal("cleanup targeted an unowned pod or omitted its UID")
		}
		return false, nil, nil
	})
	if err := backend.DeleteVMI(context.Background(), "pipeline", instance); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(podResource).Namespace("pipeline").Get(context.Background(), "launcher-false", metav1.GetOptions{}); err != nil {
		t.Fatalf("unrelated launcher was removed: %v", err)
	}
	if _, err := client.Resource(podResource).Namespace("pipeline").Get(context.Background(), "launcher-true", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned launcher survived: %v", err)
	}
}

func TestKubeVirtAmbiguousCreateRetainsUnboundIntent(t *testing.T) {
	arrived, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var object unstructured.Unstructured
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			t.Error(err)
			return
		}
		close(arrived)
		<-finish // The API may still create after the caller has timed out.
		object.SetUID("late-created")
		object.SetCreationTimestamp(metav1.Now())
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(object.Object); err != nil {
			return
		}
	}))
	defer server.Close()
	defer close(finish)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	config := transportConfig()
	config.RequestTimeout = 25 * time.Millisecond
	backend, err := NewKubeVirtBackend(client, config)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := backend.CreateVMI(context.Background(), "pipeline", validSpec())
	<-arrived
	if err == nil || instance.Name == "" || instance.SpecIdentity != SpecIdentity(validSpec()) || instance.UID != "" {
		t.Fatalf("ambiguous create lost its intent or invented ownership: %#v, %v", instance, err)
	}
	if err := backend.DeleteVMI(context.Background(), "pipeline", instance); err == nil {
		t.Fatal("unresolved intent authorized cleanup")
	}
}

func TestKubeVirtDistinguishesDefinitiveRefusalFromUnknownCreate(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		refused bool
	}{
		{"forbidden", apierrors.NewForbidden(vmiResource.GroupResource(), "vm", errors.New("denied")), true},
		{"invalid", apierrors.NewBadRequest("invalid fixed spec"), true},
		{"internal", apierrors.NewInternalError(errors.New("unknown outcome")), false},
		{"timeout", apierrors.NewTimeoutError("unknown outcome", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, client := transportFixture(t)
			client.PrependReactor("create", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, test.failure })
			intent, err := backend.CreateVMI(context.Background(), "pipeline", validSpec())
			if err == nil || errors.Is(err, ErrCreateRefused) != test.refused || intent.UID != "" || intent.Name == "" {
				t.Fatalf("create classification = %#v, %v", intent, err)
			}
		})
	}
}

func TestKubeVirtRejectsLauncherCredentialInjection(t *testing.T) {
	for _, attack := range []string{"automount", "account", "projected-token", "secret-volume", "secret-env", "none"} {
		t.Run(attack, func(t *testing.T) {
			backend, client := transportFixture(t)
			instance := createTransportVMI(t, backend)
			pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "launcher", "namespace": "pipeline", "uid": "launcher-uid"}, "spec": map[string]any{"automountServiceAccountToken": false, "serviceAccountName": "vm-guest"}}}
			controller := true
			pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: instance.Name, UID: types.UID(instance.UID), Controller: &controller}})
			var err error
			switch attack {
			case "automount":
				err = unstructured.SetNestedField(pod.Object, true, "spec", "automountServiceAccountToken")
			case "account":
				err = unstructured.SetNestedField(pod.Object, "release-credentials", "spec", "serviceAccountName")
			case "projected-token":
				err = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "token", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "token"}}}}}}, "spec", "volumes")
			case "secret-volume":
				err = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "secret", "secret": map[string]any{"secretName": "release"}}}, "spec", "volumes")
			case "secret-env":
				err = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "injected", "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "release"}}}}}, "spec", "containers")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Tracker().Create(podResource, pod, "pipeline"); err != nil {
				t.Fatal(err)
			}
			_, err = backend.GetVMI(context.Background(), "pipeline", instance.Name)
			if (err != nil) != (attack != "none") {
				t.Fatalf("launcher %s observation: %v", attack, err)
			}
		})
	}
}
