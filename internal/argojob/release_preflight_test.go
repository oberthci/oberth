package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func preflightRequest(t *testing.T) Request {
	t.Helper()
	r := wifTestRequest(t, func(wf *wfv1.Workflow) {
		wf.Annotations[argoworkflow.SecretPathsAnnotation] = "oberth/data/release/signing"
		leaf := &wf.Spec.Templates[0]
		leaf.Container.Command = []string{"oberth", "secretstore", "exec", "--path=oberth/data/release/signing", "--", "true"}
		leaf.Container.Args = nil
		other := leaf.DeepCopy()
		other.Name = "publish-other"
		wf.Spec.Templates = append(wf.Spec.Templates, *other)
	})
	r.StepGrants = map[string]map[string]bool{}
	return r
}

func TestReleasePreflightReportsEveryGrantAndWIFGap(t *testing.T) {
	r := preflightRequest(t)
	result, err := PreflightRelease(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	problems := strings.Join(result.Problems, "\n")
	for _, step := range []string{"main", "publish-other"} {
		args, _ := json.Marshal(map[string]string{"repo": "github/skipops/oberth", "step": step, "secret": "oberth/data/release/signing"})
		if !strings.Contains(problems, "access_allow "+string(args)) || !strings.Contains(problems, "step \""+step+"\": argojob: release WIF") {
			t.Fatalf("missing remediation for %s: %s", step, problems)
		}
	}
	if len(result.Declarations) != 2 {
		t.Fatalf("declarations=%+v", result.Declarations)
	}
}

func TestReleasePreflightGrantAndMaterializationAreSeparate(t *testing.T) {
	r := preflightRequest(t)
	config := wifTestConfig()
	grant := config.ReleaseWIF.Repositories["github/skipops/oberth"]
	grant.Templates["publish-other"] = "image-writer"
	r.ApprovedSecrets["oberth/data/release/signing"] = true
	r.StepGrants["oberth/data/release/signing"] = map[string]bool{"main": true, "publish-other": true}
	result, err := PreflightRelease(config, r)
	if err != nil || !strings.Contains(strings.Join(result.Problems, "\n"), "not materialized") || len(result.Checks) != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	config.PerStepIdentities = map[string]PerRepoIdentityConfig{
		"github/skipops/oberth/main":          {ServiceAccountName: "step-main"},
		"github/skipops/oberth/publish-other": {ServiceAccountName: "step-other"},
	}
	result, err = PreflightRelease(config, r)
	if err != nil || len(result.Problems) != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	kube := fake.NewClientset()
	controller := &Controller{config: config, kube: kube}
	err = controller.PreflightReleaseAccounts(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing SA: %v", err)
	}
	for _, name := range []string{"step-main", "step-other", "oberth-per-repo"} {
		_, err = kube.CoreV1().ServiceAccounts(config.Namespace).Create(context.Background(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	kube.ClearActions()
	if err := controller.PreflightReleaseAccounts(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() != "get" || action.GetResource().Resource != "serviceaccounts" {
			t.Fatalf("credential or mutation action: %v", action)
		}
	}
}

func TestReleasePreflightReportsEffectiveWIFSubject(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprint(named), func(t *testing.T) {
			r := preflightRequest(t)
			config := wifTestConfig()
			config.ReleaseWIF.Repositories["github/skipops/oberth"].Templates["publish-other"] = "image-writer"
			r.ApprovedSecrets["oberth/data/release/signing"] = true
			r.StepGrants["oberth/data/release/signing"] = map[string]bool{"*": true}
			wantSA := "oberth-per-repo"
			if named {
				r.StepGrants["oberth/data/release/signing"]["main"] = true
				config.PerStepIdentities = map[string]PerRepoIdentityConfig{
					"github/skipops/oberth/main": {ServiceAccountName: "step-main"},
				}
				wantSA = "step-main"
			}
			wf, err := argoworkflow.Decode(r.Source)
			if err != nil {
				t.Fatal(err)
			}
			wf.Spec.Templates[0].ServiceAccountName = "untrusted-authored-account"
			r.Source, err = json.Marshal(wf)
			if err != nil {
				t.Fatal(err)
			}
			result, err := PreflightRelease(config, r)
			if err != nil || len(result.Problems) != 0 {
				t.Fatalf("%+v %v", result, err)
			}
			checks := strings.Join(result.Checks, "\n")
			want := fmt.Sprintf("step %q: service_account=%q release_wif_role=%q subject=%q", "main", wantSA, "image-writer", "system:serviceaccount:"+config.Namespace+":"+wantSA)
			if !strings.Contains(checks, want) || strings.Contains(checks, "untrusted-authored-account") {
				t.Fatalf("effective identity missing or authored identity trusted: %s", checks)
			}
			if strings.Contains(checks, "cloud trust must include") != named {
				t.Fatalf("WIF warning mismatch: %s", checks)
			}
		})
	}
}

func TestReleasePreflightAccountsIgnoresAuthoredIdentity(t *testing.T) {
	r := preflightRequest(t)
	wf, err := argoworkflow.Decode(r.Source)
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].ServiceAccountName = "untrusted-authored-account"
	r.Source, err = json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	config := wifTestConfig()
	kube := fake.NewClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "oberth-per-repo", Namespace: config.Namespace}})
	controller := &Controller{config: config, kube: kube}
	if err := controller.PreflightReleaseAccounts(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, action := range kube.Actions() {
		if get, ok := action.(k8stesting.GetAction); !ok || get.GetName() != "oberth-per-repo" {
			t.Fatalf("unexpected account lookup: %v", action)
		}
	}
}
