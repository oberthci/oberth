package argoworkflow_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/pkg/argoworkflow/argoworkflowtest"
)

func TestRealShapePlanGroupsStayAdjacent(t *testing.T) {
	plan := argoworkflowtest.Plan(t)
	levels := []string{
		"residue-lint", "contract-canonical", "surface-registry", "contract-guard", "cohort", "profiles",
		"release-index tools-dir deprecations",
		"link-go install-golangci-lint install-cosign verify-helm-schema",
		"install-systemd-tools install-release-support mod-tidy-check mod-verify prepare-openbao-cache vet test-ia32-probe test-product-probes test-install-probes security",
		"scan-systemd-tools verify-release-tools golangci-lint verify-tls-fingerprint-imports paired-chart verify-version-pins",
		"smoke-port-chart-index test", "test-systemd-budget demo-server-build fixture-cosign",
		"build-linux-amd64 build-linux-arm64 trivy-fixture-build",
		"build-darwin-amd64 build-darwin-arm64 trivy-db-update",
		"trivy-fixture-qualify", "bao-fixture-build", "bao-fixture-controls", "test-openbao",
	}
	assertRealLevels(t, plan, levels)
}

func assertRealLevels(t *testing.T, plan admissioncontract.Plan, levels []string) {
	t.Helper()
	index, previous := 0, 0
	for _, level := range levels {
		names := strings.Fields(level)
		group := plan.Entries[index].ParallelGroup
		if (len(names) > 1) != (group > 0) || group > 0 && group == previous {
			t.Fatal("levels share a group", level, plan)
		}
		for _, name := range names {
			entry := plan.Entries[index]
			if entry.Path != name || entry.ParallelGroup != group {
				t.Fatal("interleaved real DAG", name, entry)
			}
			index++
		}
		previous = group
	}
	if index != 44 || index != len(plan.Entries) {
		t.Fatal("fixture lost tasks", index, len(plan.Entries))
	}
}
