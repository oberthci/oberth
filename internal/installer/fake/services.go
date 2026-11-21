package fake

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// DryRunService identifies non-persistent Service validation requests.
func DryRunService(action ktesting.Action) bool {
	if action.GetResource().Resource != "services" {
		return false
	}
	switch a := action.(type) {
	case ktesting.CreateActionImpl:
		return slices.Equal(a.GetCreateOptions().DryRun, []string{metav1.DryRunAll})
	case ktesting.UpdateActionImpl:
		return slices.Equal(a.GetUpdateOptions().DryRun, []string{metav1.DryRunAll})
	}
	return false
}

// DryRunServices keeps the fake tracker from persisting server-side dry runs.
func DryRunServices(client *kubefake.Clientset) {
	client.PrependReactor("*", "services", func(action ktesting.Action) (bool, runtime.Object, error) {
		if !DryRunService(action) {
			return false, nil, nil
		}
		switch a := action.(type) {
		case ktesting.CreateActionImpl:
			return true, a.GetObject().DeepCopyObject(), nil
		case ktesting.UpdateActionImpl:
			return true, a.GetObject().DeepCopyObject(), nil
		}
		return false, nil, nil
	})
}
