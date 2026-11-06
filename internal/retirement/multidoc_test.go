package retirement

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRetirementMultiDocumentCoverage(t *testing.T) {
	const ordinary = "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec: {entrypoint: main, templates: [{name: main, container: {image: busybox, command: [true]}}]}\n"
	for _, tc := range []struct {
		name, tail, want string
		capability       bool
	}{
		{name: "retained workflow and config", tail: "on: {push: {branches: [main]}}\nworkflows: {post-merge: {template: ci}}\n"},
		{name: "later legacy annotation", tail: "metadata: {annotations: {oberth.ci/kvm-templates: main}}", want: "document 2: legacy declaration"},
		{name: "later legacy environment", tail: "spec: {env: [{name: OBERTH_GUEST_TEST, value: '1'}]}", want: "document 2: legacy declaration"},
		{name: "third legacy document", tail: "kind: ConfigMap\n---\nmetadata: {annotations: {oberth.ci/cleanup-verdict: 60s}}", want: "document 3: legacy declaration"},
		{name: "later device without authority", tail: "resources: {limits: {devices.kubevirt.io/kvm: 1}}", want: "document 2: legacy KubeVirt"},
		{name: "later device with authority", tail: "resources: {limits: {devices.kubevirt.io/kvm: 1}}", capability: true},
		{name: "later resource without authority", tail: "resource: {manifest: 'apiVersion: kubevirt.io/v1'}", want: "document 2: legacy KubeVirt"},
		{name: "later invalid yaml", tail: "spec: [", want: "document 2: inspect pipeline YAML"},
		{name: "later duplicate key", tail: "kind: Workflow\nkind: Workflow", want: "document 2: inspect pipeline YAML"},
		{name: "later scalar", tail: "not-an-object", want: "document 2: pipeline must be a YAML object"},
		{name: "later array", tail: "[{}, {}]", want: "document 2: pipeline must be a YAML object"},
		{name: "later null", tail: "null", want: "document 2: pipeline must be a YAML object"},
		{name: "later unnamed fragment", tail: "templateRef: {}", want: "document 2: cannot inspect unnamed pipeline fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := []byte(ordinary + "---\n" + tc.tail)
			refs, err := CheckSource(".oberth/ci.yaml", source, tc.capability)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || refs != nil {
				t.Fatalf("partial/incorrect proof: %v %v; want %s", refs, err, tc.want)
			}
			// Inventory acceptance never authorizes execution of either document.
			if _, err := argoworkflow.Decode(source); err == nil {
				t.Fatal("execution decoder accepted multi-document input")
			}
		})
	}
}

func TestRetirementMultiDocumentReferencesAndBounds(t *testing.T) {
	first := "spec: {templates: [{templateRef: {name: 'forge/team/first@v1'}}]}\n"
	second := "spec: {templates: [{templateRef: {name: 'forge/team/second@v1'}}]}\n"
	refs, err := CheckSource(".oberth/ci.yaml", []byte(first+"---\n"+second+"---\n"+first), false)
	if err != nil || !reflect.DeepEqual(refs, []string{"forge/team/first@v1", "forge/team/second@v1"}) {
		t.Fatalf("not all documents inspected: %v %v", refs, err)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"no document", "", "at least one YAML document"},
		{"source bytes", strings.Repeat("x", (1<<20)+1), "source exceeds"},
		{"document count", strings.Repeat("kind: ConfigMap\n---\n", 257), "document count exceeds 256"},
		{"later nesting", first + "---\nroot: " + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), "document 2: retirement inspection nesting exceeds 64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := CheckSource(".oberth/ci.yaml", []byte(tc.body), false)
			if err == nil || !strings.Contains(err.Error(), tc.want) || refs != nil {
				t.Fatalf("bound did not refuse: %v %v", refs, err)
			}
		})
	}
	if _, err := CheckSource(".oberth/ci.yaml", []byte(strings.Repeat("kind: ConfigMap\n---\n", 256)), false); err != nil {
		t.Fatal("exact document limit refused", err)
	}
}

func TestRetirementMultiDocumentDoesNotRelaxProtectedContract(t *testing.T) {
	body := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [program]\n---\nkind: ConfigMap\n")
	if _, err := CheckSource(protectedtests.ContractFile, body, true); err == nil {
		t.Fatal("standalone census accepted protected contract")
	}
	if _, err := protectedtests.DecodeContract(body); err == nil {
		t.Fatal("protected contract decoder accepted additional document")
	}
}
