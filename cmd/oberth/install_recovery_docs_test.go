package main

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallGuideStatesEncryptedCoverageAndReviewLimits(t *testing.T) {
	raw, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := string(raw)
	for heading, required := range map[string][]string{
		"Keep the recovery files": {"five installation Secrets", "encrypt", "bundled store data", "recovery", "age identity", "administrator private file"},
		"Back up": {"five installation Secrets", "CA key", "forge deploy key", "SSH host key", "store TLS", "administrator",
			"--database-only", "--print-schedule", "--yes", "archive has neither", "Keep age"},
	} {
		_, section, found := strings.Cut(guide, "## "+heading+"\n")
		section, _, _ = strings.Cut(section, "\n## ")
		if !found {
			t.Fatal("missing recovery section", heading)
		}
		section = strings.Join(strings.Fields(section), " ")
		for _, text := range required {
			if !strings.Contains(section, text) {
				t.Error(heading, "omits", text)
			}
		}
	}
	for _, command := range []string{"oberth backup --out <new-archive-file>", "oberth restore --from <archive> --identity-file <private-age-file> --dry-run"} {
		if !strings.Contains(guide, command) {
			t.Fatal("encrypted backup or offline review command missing", command)
		}
	}
}

func TestInstallerRecoveryRemediesNameAvailableActions(t *testing.T) {
	for code, action := range map[string]string{
		"schema-downgrade":      "oberth upgrade --version <version>",
		"release-provenance":    "storage level",
		"chart-state":           "helm history oberth -n <namespace>",
		"identity-missing":      "saved private copy",
		"administrator-missing": "saved private copy of oberth-admin",
		"release-unowned":       "storage-level copy",
	} {
		remedy := installer.NewProblem(code, nil, "").Remedy
		if !strings.Contains(remedy, action) || strings.Contains(remedy, "recover into") || strings.Contains(remedy, "from backup") {
			t.Error(code, remedy)
		}
	}
}

func TestInstallGuideRequiresVolumeCopyBeforeSchemaUpgrade(t *testing.T) {
	raw, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	_, upgrade, found := strings.Cut(string(raw), "## Upgrade\n")
	upgrade, _, _ = strings.Cut(upgrade, "\n## ")
	beforeApply, _, applies := strings.Cut(upgrade, "oberth upgrade --yes")
	if !found || !applies {
		t.Fatal("upgrade guide lacks its reviewed apply sequence")
	}
	text := strings.Join(strings.Fields(beforeApply), " ")
	if !strings.Contains(text, "When the review shows a schema change, take a storage-level copy of the data volume before applying.") {
		t.Fatal("guide does not require a recoverable copy before applying a schema change")
	}
}
