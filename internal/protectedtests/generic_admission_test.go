package protectedtests

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestGenericAdmissionKeepsPinnedHostWireAndCopiesAuthority(t *testing.T) {
	e, _, _ := handoffFixture()
	c := &argoworkflow.Capability{Namespace: e.Intent.Namespace, ServiceAccount: "operator-runner", ExtendedResources: map[string]int64{GenericDeviceResource: 1}, ResourceKinds: []argoworkflow.ResourceKindCapability{{APIVersion: "tasks.example.org/v1", Kind: "Task", Actions: []string{"delete", "create"}}}}
	before, _ := json.Marshal(e.Intent)
	a, err := NewGenericAdmission(e.Intent, c, 1234)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(a.Intent)
	if string(before) != string(after) {
		t.Fatal("generic metadata changed pinned host wire")
	}
	if err = a.MatchCapability(c); err != nil {
		t.Fatal(err)
	}
	c.ResourceKinds[0].Actions = []string{"create", "delete"}
	if err = a.MatchCapability(c); err != nil {
		t.Fatal("equivalent authority reordering rejected")
	}
	digest := a.Digest()
	c.ExtendedResources[GenericDeviceResource] = 2
	c.ResourceKinds[0].Actions[0] = "get"
	e.Intent.Contract.Artifacts[0] = "replacement"
	if a.Digest() != digest {
		t.Fatal("caller mutated sealed admission")
	}
	if err = a.MatchCapability(c); err == nil {
		t.Fatal("changed authority accepted")
	}
	if err = a.MatchCapability(nil); err == nil {
		t.Fatal("revoked authority accepted")
	}
	for name, change := range map[string]func(*GenericAdmission){
		"version":         func(a *GenericAdmission) { a.Version = 2 },
		"namespace":       func(a *GenericAdmission) { a.Capability.Namespace = "foreign" },
		"account":         func(a *GenericAdmission) { a.Capability.ServiceAccount = "default" },
		"no device":       func(a *GenericAdmission) { a.Capability.ExtendedResources = nil },
		"zero group":      func(a *GenericAdmission) { a.SupplementalGroup = 0 },
		"oversized group": func(a *GenericAdmission) { a.SupplementalGroup = 2147483648 },
		"bad intent":      func(a *GenericAdmission) { a.Intent.CandidateSHA = "unbound" },
	} {
		t.Run(name, func(t *testing.T) {
			copy, err := NewGenericAdmission(a.Intent, &a.Capability, a.SupplementalGroup)
			if err != nil {
				t.Fatal(err)
			}
			change(&copy)
			if copy.Validate() == nil {
				t.Fatal("invalid admission accepted")
			}
		})
	}
}
