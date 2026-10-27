package argojob

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGenericWIFAccountPreflightNeedsNoSecretGrantOrNewPermissions(t *testing.T) {
	c := genericWIFConfig()
	r := genericWIFRequest(t, nil)
	kube := fake.NewClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "oberth-per-repo", Namespace: c.Namespace}}, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "release-step-main", Namespace: c.Namespace}})
	controller := &Controller{config: c, kube: kube}
	if err := controller.PreflightReleaseAccounts(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() != "get" || action.GetResource().Resource != "serviceaccounts" {
			t.Fatalf("preflight requested new authority: %v", action)
		}
	}
	if err := kube.CoreV1().ServiceAccounts(c.Namespace).Delete(t.Context(), "release-step-main", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := controller.PreflightReleaseAccounts(t.Context(), r); err == nil || !strings.Contains(err.Error(), "WIF-only ServiceAccount") {
		t.Fatalf("missing pinned identity ignored: %v", err)
	}
}

func TestGenericWIFRejectsReconciledOtherStepAlias(t *testing.T) {
	c := genericWIFConfig()
	r := genericWIFRequest(t, nil)
	identities := NewIdentityStoreWithSteps(c.PerRepoIdentities, c.PerRepoCIIdentities, map[string]PerRepoIdentityConfig{"github/org/repo/other": {ServiceAccountName: "release-step-main"}})
	r.Identities = identities.Snapshot()
	if _, err := Build(c, r); err == nil {
		t.Fatal("reconciled other-step alias obtained the WIF identity")
	}
}
