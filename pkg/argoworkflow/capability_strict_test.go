package argoworkflow

import "testing"

func TestCapabilityStrictDecodingKeepsExistingPolicyOnRefusal(t *testing.T) {
	t.Parallel()
	valid := `{"namespace":"runs","service_account":"resource","resource_kinds":[{"api_version":"v1","kind":"ConfigMap","actions":["create"]}]}`
	p := Policy{ServerNamespace: "oberth", ArgoNamespace: "argo"}
	if err := p.LoadCapability(t.Context(), "ci", []byte(valid)); err != nil {
		t.Fatal(err)
	}
	previous := p.Capability
	for _, raw := range []string{
		`{"namespace":"runs","service_account":"resource","unknown":true}`,
		`{"namespace":"runs","service_account":"resource","resource_kinds":[{"api_version":"v1","kind":"ConfigMap","actions":["create"],"unknown":true}]}`,
		valid + ` {}`, `null`,
	} {
		if err := p.LoadCapability(t.Context(), "ci", []byte(raw)); err == nil || p.Capability != previous {
			t.Fatal("non-strict capability replaced policy", raw, err)
		}
	}
}
