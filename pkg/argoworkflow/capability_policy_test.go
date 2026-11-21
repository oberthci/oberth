package argoworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestCapabilityGrantUsesAdmissionPolicy(t *testing.T) {
	t.Parallel()
	p := Policy{ServerNamespace: "oberth", ArgoNamespace: "controller", Capability: &Capability{Namespace: "runs", ServiceAccount: "resource", ResourceKinds: []ResourceKind{{APIVersion: "v1", Kind: "ConfigMap", Actions: []string{"create"}}}}}
	if err := checkCapability(t, p); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"oberth", "controller", "ci"} {
		p.Capability.Namespace = ns
		if err := checkCapability(t, p); err == nil {
			t.Fatal("protected namespace granted", ns)
		}
	}
	p.Capability.Namespace = "runs"
	p.Capability.ResourceKinds[0].Kind = "ClusterRole"
	p.Capability.ResourceKinds[0].APIVersion = "rbac.authorization.k8s.io/v1"
	if err := checkCapability(t, p); err == nil {
		t.Fatal("cluster authority granted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.LoadCapability(ctx, "ci", []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled capability check ignored", err)
	}
}

func checkCapability(t *testing.T, p Policy) error {
	t.Helper()
	raw, err := json.Marshal(p.Capability)
	if err != nil {
		t.Fatal(err)
	}
	return p.LoadCapability(t.Context(), "ci", raw)
}

func TestCapabilityDecodeRefusalDoesNotPublishTheProposal(t *testing.T) {
	t.Parallel()
	valid := `{"namespace":"runs","service_account":"resource","resource_kinds":[{"api_version":"v1","kind":"ConfigMap","actions":["create"]}]}`
	for _, raw := range []string{`{`, `null`, strings.Repeat(" ", admissioncontract.MaxDocumentBytes) + valid} {
		p := Policy{ServerNamespace: "oberth", ArgoNamespace: "controller"}
		if err := p.LoadCapability(t.Context(), "ci", []byte(raw)); err == nil || p.Capability != nil {
			t.Fatal("refused capability was applied", err)
		}
	}
}

func TestCapabilityCannotTargetPipelineNamespace(t *testing.T) {
	t.Parallel()
	wf, fixture := resourceWorkflow("apiVersion: v1\nkind: Pod\nmetadata: {name: foreign-step, namespace: ci}\nspec:\n  serviceAccountName: other-repository-step\n")
	fixture.Capability.Namespace = "ci"
	fixture.Capability.ResourceKinds = []ResourceKind{{APIVersion: "v1", Kind: "Pod", Actions: []string{"create"}}}
	q := requestFor(t, wf)
	q.IdentityRequest.ResourceAccount = "resources"
	result, err := prepareFixture(t.Context(), q, fixture, identityHooks())
	requireCode(t, err, "protected_namespace")
	if result != nil {
		t.Fatal("refusal published a prepared workflow")
	}
}
