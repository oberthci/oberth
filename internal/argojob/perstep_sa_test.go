package argojob

import (
	"context"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// ---------------------------------------------------------------------------
// verifyPerStepServiceAccounts tests (issue #623, finding 2)
// ---------------------------------------------------------------------------

const (
	perStepTestNamespace  = "oberth-pipeline"
	perStepTestPipeline   = "oberth-argo-pipeline"
	perStepTestCred       = "oberth-argo-credentialed"
	perStepTestCISecrets  = "oberth-argo-ci-secrets"
	perStepTestExecutor   = "oberth-argo-executor"
	perStepTestPerStep    = "oberth-argo-step-publish-abc123"
	perStepTestPerStepAlt = "oberth-argo-step-deploy-def456"
)

func perStepConfig() Config {
	return Config{
		Namespace:                  perStepTestNamespace,
		PipelineServiceAccount:     perStepTestPipeline,
		CredentialedServiceAccount: perStepTestCred,
		CISecretsServiceAccount:    perStepTestCISecrets,
		ExecutorServiceAccount:     perStepTestExecutor,
	}
}

// perStepWorkflow builds a workflow with templates referencing the given SAs.
// workflowSA is the workflow-level ServiceAccountName; templateSAs maps
// template name to its ServiceAccountName override.
func perStepWorkflow(workflowSA string, templateSAs map[string]string) *wfv1.Workflow {
	wf := &wfv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "test-wf", Namespace: perStepTestNamespace},
		Spec: wfv1.WorkflowSpec{
			ServiceAccountName: workflowSA,
		},
	}
	for name, sa := range templateSAs {
		wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{
			Name:               name,
			ServiceAccountName: sa,
		})
	}
	return wf
}

func TestVerifyPerStepServiceAccounts_MissingSADenied(t *testing.T) {
	t.Parallel()

	// No per-step SA exists in the fake cluster.
	kube := fake.NewClientset()
	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"publish": perStepTestPerStep,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err == nil {
		t.Fatal("expected denial for missing per-step SA")
	}

	// Verify the exact denial text format.
	want := fmt.Sprintf("per-step ServiceAccount %q does not exist in namespace %q", perStepTestPerStep, perStepTestNamespace)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("denial message mismatch:\n  got:  %s\n  want substring: %s", err, want)
	}
	if !strings.Contains(err.Error(), "oberth secretstore sync") {
		t.Fatalf("denial should mention `oberth secretstore sync`, got: %s", err)
	}
}

func TestVerifyPerStepServiceAccounts_PresentSAAccepted(t *testing.T) {
	t.Parallel()

	// Create the per-step SA in the fake cluster.
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      perStepTestPerStep,
			Namespace: perStepTestNamespace,
		},
	}
	kube := fake.NewClientset(sa)
	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"publish": perStepTestPerStep,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err != nil {
		t.Fatalf("expected nil for present SA, got: %v", err)
	}
}

func TestVerifyPerStepServiceAccounts_WorkflowSASkipped(t *testing.T) {
	t.Parallel()

	// No SAs in the cluster — but template SA equals workflow SA, so it
	// should be skipped entirely.
	kube := fake.NewClientset()

	// Track API calls to verify no SA lookup was made.
	var lookups []string
	kube.PrependReactor("get", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		getAction := action.(ktesting.GetAction)
		lookups = append(lookups, getAction.GetName())
		return false, nil, nil
	})

	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	// Template SA equals workflow-level SA.
	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"build": perStepTestPipeline,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err != nil {
		t.Fatalf("expected nil when template SA equals workflow SA, got: %v", err)
	}
	if len(lookups) > 0 {
		t.Fatalf("should not have looked up SA %v when it equals the workflow SA", lookups)
	}
}

func TestVerifyPerStepServiceAccounts_SharedSAsSkipped(t *testing.T) {
	t.Parallel()

	kube := fake.NewClientset()

	var lookups []string
	kube.PrependReactor("get", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		getAction := action.(ktesting.GetAction)
		lookups = append(lookups, getAction.GetName())
		return false, nil, nil
	})

	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	// Templates using each of the four shared SAs.
	wf := perStepWorkflow("custom-workflow-sa", map[string]string{
		"step-pipeline":   perStepTestPipeline,
		"step-cred":       perStepTestCred,
		"step-ci-secrets": perStepTestCISecrets,
		"step-executor":   perStepTestExecutor,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err != nil {
		t.Fatalf("expected nil for shared SAs, got: %v", err)
	}
	if len(lookups) > 0 {
		t.Fatalf("shared SAs should not be looked up, but got lookups: %v", lookups)
	}
}

func TestVerifyPerStepServiceAccounts_NonNotFoundAPIError(t *testing.T) {
	t.Parallel()

	kube := fake.NewClientset()

	// Inject a non-NotFound API error.
	kube.PrependReactor("get", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})

	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"deploy": perStepTestPerStep,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err == nil {
		t.Fatal("expected error for non-NotFound API failure")
	}
	// Should be a wrapped error, not the denial text.
	if strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("non-NotFound error should not produce denial text, got: %s", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected wrapped API error, got: %s", err)
	}
}

func TestVerifyPerStepServiceAccounts_NilKubeIsNoOp(t *testing.T) {
	t.Parallel()

	// When kube is nil (test mode), verification is a no-op.
	controller := &Controller{
		kube:   nil,
		config: perStepConfig(),
	}

	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"publish": perStepTestPerStep,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err != nil {
		t.Fatalf("nil kube should be no-op, got: %v", err)
	}
}

func TestVerifyPerStepServiceAccounts_DuplicateSACheckedOnce(t *testing.T) {
	t.Parallel()

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      perStepTestPerStep,
			Namespace: perStepTestNamespace,
		},
	}
	kube := fake.NewClientset(sa)

	var lookupCount int
	kube.PrependReactor("get", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		getAction := action.(ktesting.GetAction)
		if getAction.GetName() == perStepTestPerStep {
			lookupCount++
		}
		return false, nil, nil
	})

	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	// Two templates reference the same per-step SA.
	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"publish-arm64": perStepTestPerStep,
		"publish-amd64": perStepTestPerStep,
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
	if lookupCount != 1 {
		t.Fatalf("duplicate SA should be checked only once, got %d lookups", lookupCount)
	}
}

func TestVerifyPerStepServiceAccounts_EmptySASkipped(t *testing.T) {
	t.Parallel()

	kube := fake.NewClientset()
	controller := &Controller{
		kube:   kube,
		config: perStepConfig(),
	}

	// Template with no ServiceAccountName set (inherits workflow-level).
	wf := perStepWorkflow(perStepTestPipeline, map[string]string{
		"lint": "",
	})

	err := controller.verifyPerStepServiceAccounts(context.Background(), wf)
	if err != nil {
		t.Fatalf("empty template SA should be skipped, got: %v", err)
	}
}
