// Executed in a disposable copy of the pinned Argo controller package.
package controller

import (
	"encoding/json"
	"flag"
	"os"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOberthParentMountLeavesHelperAccessible(t *testing.T) {
	if len(flag.Args()) != 1 {
		t.Fatal("pass compiled Workflows after -args")
	}
	raw, err := os.ReadFile(flag.Args()[0])
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Parent   string
		Workflow *wfv1.Workflow
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 10 {
		t.Fatal("missing parent/retry cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Parent, func(t *testing.T) {
			ctx := logging.TestContext(t.Context())
			wf := c.Workflow
			woc := newWoc(ctx, *wf)
			for _, name := range []string{wf.Spec.ServiceAccountName, wf.Spec.Executor.ServiceAccountName} {
				if _, err := woc.controller.kubeclientset.CoreV1().ServiceAccounts(wf.Namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace}}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			tmpl := wf.GetTemplateByName("test")
			pod, err := woc.createWorkflowPod(ctx, wf.Name, []corev1.Container{*tmpl.Container}, tmpl, &createWorkflowPodOpts{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, main := range pod.Spec.Containers {
				if main.Name != "main" {
					continue
				}
				found = true
				parent, helper := -1, -1
				for i, m := range main.VolumeMounts {
					if m.Name == "parent" && m.MountPath == c.Parent {
						parent = i
					}
					if m.MountPath == "/run/oberth/assist" && m.Name == "oberth-source" && m.ReadOnly && m.SubPath == "bin" {
						helper = i
					}
				}
				if parent < 0 || helper <= parent {
					t.Fatal("actual Pod lost parent-before-helper ordering", main.VolumeMounts)
				}
			}
			if !found {
				t.Fatal("missing main")
			}
			t.Log("actual Argo Pod preserves parent-before-helper mount ordering")
		})
	}
}
