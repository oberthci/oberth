package retirement

import (
	"strings"
	"testing"
)

func TestLegacyRemovalRefusesDeclarations(t *testing.T) {
	for _, fixture := range []struct{ path, body, name string }{
		{".oberth/tests.yaml", "version: 2\n", "tests.yaml"},
		{".oberth/build.yaml", "metadata: {annotations: {oberth.ci/kvm-templates: main}}", "kvm-templates"},
		{".oberth/release.yaml", "metadata: {annotations: {oberth.ci/guest-test-templates: main}}", "guest-test-templates"},
		{".oberth/fragment.yaml", "spec: {templates: [{container: {env: [{name: OBERTH_KVM, value: '1'}]}}]}", "OBERTH_KVM"},
		{".oberth/nightly.yaml", "spec: {templates: [{container: {env: [{name: OBERTH_GUEST_TEST, value: '1'}]}}]}", "OBERTH_GUEST_TEST"},
		{".oberth/build.yaml", "spec: {templates: [{resource: {manifest: 'apiVersion: kubevirt.io/v1\nkind: VirtualMachineInstance'}}]}", "KubeVirt"},
		{".oberth/build.yaml", "metadata: {annotations: {oberth.ci/capability: forged}}\nspec: {templates: [{resource: {manifest: 'apiVersion: kubevirt.io/v1\nkind: VirtualMachineInstance'}}]}", "KubeVirt"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			_, err := CheckSource(fixture.path, []byte(fixture.body), false)
			if err == nil || !strings.Contains(err.Error(), fixture.name) {
				t.Fatalf("declaration accepted: %v", err)
			}
		})
	}
}

func TestLegacyRemovalReadsPinnedFragmentsAndRejectsMalformedSource(t *testing.T) {
	refs, err := CheckSource(".oberth/build.yaml", []byte("spec: {templates: [{steps: [[{templateRef: {name: 'forge/org/library@v1', template: compile}}]]}]}"), false)
	if err != nil || len(refs) != 1 || refs[0] != "forge/org/library@v1" {
		t.Fatalf("fragment proof omitted: %v %v", refs, err)
	}
	for _, body := range []string{"spec: [", "metadata: {}\nmetadata: {}", "---\nkind: Workflow\n---\nkind: Workflow"} {
		if _, err := CheckSource(".oberth/build.yaml", []byte(body), false); err == nil {
			t.Fatal("malformed/ambiguous source accepted")
		}
	}
}

func TestLegacyRemovalAllowsOrdinaryAndCapabilitySources(t *testing.T) {
	for _, body := range []string{
		"# OBERTH_KVM is retired\nkind: Workflow\nspec: {templates: [{container: {args: ['ordinary']}}]}",
		"metadata: {annotations: {oberth.ci/capability: operator-task}}\nspec: {templates: [{resource: {manifest: 'apiVersion: kubevirt.io/v1\nkind: VirtualMachineInstance'}}]}",
	} {
		if _, err := CheckSource(".oberth/build.yaml", []byte(body), true); err != nil {
			t.Fatal(err)
		}
	}
}
