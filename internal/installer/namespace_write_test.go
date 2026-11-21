package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestNamespaceOwnerChangesAtWrite(t *testing.T) {
	t.Parallel()
	for _, evidence := range []string{"marks", "release"} {
		t.Run(evidence, func(t *testing.T) { namespaceOwnerAtWriteCase(t, evidence) })
	}
}
func namespaceOwnerAtWriteCase(t *testing.T, evidence string) {
	t.Helper()
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", HTTPSNodePort: 31443, SSHNodePort: 31022}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "second"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, f)
	_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), func(phase installer.Phase) error {
		if phase != installer.ReleaseChecked {
			return nil
		}
		if evidence == "release" {
			addMembershipRelease(t, f, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: second\n")
		} else {
			ns, err := f.client.CoreV1().Namespaces().Get(t.Context(), "second", metav1.GetOptions{})
			if err != nil {
				return err
			}
			ns.Annotations = map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "first"}
			if _, err = f.client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
		f.client.ClearActions()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "another Helm release") {
		t.Fatal("fresh membership was ignored", err)
	}
	assertNoInstallWrites(t, f)
	if evidence == "marks" {
		ns, err := f.client.CoreV1().Namespaces().Get(t.Context(), "second", metav1.GetOptions{})
		if err != nil || ns.Annotations["meta.helm.sh/release-namespace"] != "first" {
			t.Fatal("foreign owner changed", err)
		}
	}
	assertNoNamespaceSecrets(t, f, "second")
}

func TestNamespaceCreateRaceIsNeverAdopted(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Namespace = "second"
	plan := prepare(t, f)
	f.client.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
		ns := action.(ktesting.CreateAction).GetObject().(*corev1.Namespace).DeepCopy()
		ns.Annotations["meta.helm.sh/release-namespace"] = "first"
		if err := f.client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("namespaces"), ns, ""); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, ns.Name)
	})
	_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatal("create conflict was adopted", err)
	}
	ns, err := f.client.CoreV1().Namespaces().Get(t.Context(), "second", metav1.GetOptions{})
	if err != nil || ns.Annotations["meta.helm.sh/release-namespace"] != "first" {
		t.Fatal("racing namespace changed", err)
	}
	if len(f.charts.Applies) != 0 {
		t.Fatal("release written after failed create")
	}
	for _, action := range f.client.Actions() {
		if action.GetVerb() == "update" && action.GetResource().Resource == "namespaces" {
			t.Fatal("namespace updated", action)
		}
	}
	assertNoNamespaceSecrets(t, f, "second")
}
func assertNoNamespaceSecrets(t *testing.T, f fixture, namespace string) {
	t.Helper()
	list, err := f.client.CoreV1().Secrets(namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(list.Items) != 0 {
		t.Fatal("identity published after namespace refusal", err)
	}
}
