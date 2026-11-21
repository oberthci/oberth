package argoworkflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestCapabilityNeedsRespectProtectedNamespaces(t *testing.T) {
	for _, namespace := range []string{"oberth", "argo", "ci", "deploy"} {
		t.Run(namespace, func(t *testing.T) { assertCapabilityNamespaceNeed(t, namespace) })
	}
}

func assertCapabilityNamespaceNeed(t *testing.T, namespace string) {
	t.Helper()
	w, fixture := resourceWorkflow("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: note, namespace: " + namespace + "}")
	policy := fixture.Policy
	policy.ServerNamespace, policy.ArgoNamespace = "oberth", "argo"
	capability := Capability{Namespace: namespace, ServiceAccount: "resource-account", ResourceKinds: []ResourceKind{{APIVersion: "v1", Kind: "ConfigMap", Actions: []string{"create"}}}}
	raw, err := json.Marshal(capability)
	if err != nil {
		t.Fatal(err)
	}
	grantErr := policy.LoadCapability(t.Context(), requestFor(t, w).IdentityRequest.Namespace, raw)
	protected := grantErr != nil
	if protected != (namespace != "deploy") {
		t.Fatal("protected namespace list differs", namespace, grantErr)
	}
	if protected {
		requireCode(t, grantErr, "protected_namespace")
	}
	policy.Capability = nil
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeSubmit, admissioncontract.ModeInspect} {
		r := requestFor(t, w)
		r.Mode, r.IdentityRequest.Mode = mode, mode
		hooks := identityHooks()
		if mode == admissioncontract.ModeInspect {
			hooks = admissioncontract.PrepareHooks{OfflineValidation: true}
		}
		prepared, err := Prepare(t.Context(), r, policy, hooks)
		var findings []admissioncontract.Finding
		if protected || mode == admissioncontract.ModeSubmit {
			findings = admissionFindings(t, err)
		} else {
			if err != nil {
				t.Fatal(err)
			}
			findings = prepared.View().Findings
		}
		assertCapabilityNamespaceRemedy(t, findings, protected)
	}
}

func assertCapabilityNamespaceRemedy(t *testing.T, findings []admissioncontract.Finding, protected bool) {
	t.Helper()
	refused, command := false, false
	for _, f := range findings {
		refused = refused || f.Code == "protected_namespace"
		command = command || strings.Contains(f.Command, "oberth workflow capability grant")
	}
	if refused != protected || command == protected {
		t.Fatal("capability remedy disagrees with the grant", findings)
	}
}
