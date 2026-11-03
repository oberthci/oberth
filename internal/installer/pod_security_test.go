package installer

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func TestInstallPreflightsBothPodSecurityNamespaces(t *testing.T) {
	for _, ns := range []string{"server-custom", "pipeline-custom"} {
		for _, level := range []string{"baseline", "restricted"} {
			t.Run(ns+"/"+level, func(t *testing.T) {
				client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name: ns, Labels: map[string]string{"pod-security.kubernetes.io/enforce": level},
				}}, readyArgoControllerPod())
				var out bytes.Buffer
				writes := 0
				deps := Deps{KubeClient: client, Output: &out,
					RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
					RunHelm: func(_ context.Context, args []string) ([]byte, error) {
						if args[0] != "list" && args[0] != "get" {
							writes++
						}
						return []byte(`[]`), nil
					},
				}
				err := Run(t.Context(), Config{DryRun: true, Namespace: "server-custom", ArgoNamespace: "pipeline-custom"}, deps)
				if err == nil || !strings.Contains(err.Error(), ns) || !strings.Contains(err.Error(), "pod-security.kubernetes.io/enforce=privileged") {
					t.Fatalf("expected actionable PSA refusal for %s: %v", ns, err)
				}
				if writes != 0 {
					t.Fatalf("preflight performed %d Helm writes", writes)
				}
				for _, action := range client.Actions() {
					if action.GetVerb() != "get" && action.GetVerb() != "list" {
						t.Fatalf("preflight wrote: %v", action)
					}
				}
			})
		}
	}
}

func TestPodSecurityNamespaceCreationAndDryRun(t *testing.T) {
	for _, create := range []bool{false, true} {
		client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "server", Labels: map[string]string{"owner": "operator"}}})
		cfg := Config{Namespace: "server", ArgoNamespace: "pipeline"}
		if err := preflightPodSecurity(t.Context(), cfg, Deps{KubeClient: client}, create); err != nil {
			t.Fatal(err)
		}
		ns, err := client.CoreV1().Namespaces().Get(t.Context(), "pipeline", metav1.GetOptions{})
		if create {
			if err != nil || ns.Labels["pod-security.kubernetes.io/enforce"] != "privileged" {
				t.Fatalf("missing explicit PSA label: %v %v", ns, err)
			}
		} else if err == nil {
			t.Fatal("dry run created namespace")
		}
		server, err := client.CoreV1().Namespaces().Get(t.Context(), "server", metav1.GetOptions{})
		if err != nil || len(server.Labels) != 1 || server.Labels["owner"] != "operator" {
			t.Fatalf("existing namespace modified: %v %v", server, err)
		}
	}
}

func TestPodSecurityPreflightRefusalBeforeNamespaceCreation(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pipeline", Labels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}}})
	err := preflightPodSecurity(t.Context(), Config{Namespace: "missing-server", ArgoNamespace: "pipeline"}, Deps{KubeClient: client}, true)
	if err == nil {
		t.Fatal("restricted pipeline accepted")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("changed namespace before complete preflight: %v", action)
		}
	}
}

func TestPodSecurityConcurrentNamespaceCreationIsRechecked(t *testing.T) {
	client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "server"}})
	client.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
		ns := action.(ktesting.CreateAction).GetObject().(*corev1.Namespace).DeepCopy()
		ns.Labels[podSecurityEnforceLabel] = "restricted"
		if err := client.Tracker().Add(ns); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, ns.Name)
	})
	err := preflightPodSecurity(t.Context(), Config{Namespace: "server", ArgoNamespace: "pipeline"}, Deps{KubeClient: client}, true)
	if err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("concurrent restrictive policy accepted: %v", err)
	}
}

func TestPodSecurityNamespaceReadFailureIsClosed(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("get", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "server", nil)
	})
	err := preflightPodSecurity(t.Context(), Config{Namespace: "server", ArgoNamespace: "pipeline"}, Deps{KubeClient: client}, true)
	if err == nil || !strings.Contains(err.Error(), "server") {
		t.Fatalf("failed read accepted: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("write after failed read: %v", action)
		}
	}
}
