package main

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallGuideStatesRecoveryLimitsAndSavesFiveSecrets(t *testing.T) {
	raw, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := string(raw)
	for _, heading := range []string{"Keep the recovery files", "Back up"} {
		_, section, found := strings.Cut(guide, "## "+heading+"\n")
		section, _, _ = strings.Cut(section, "\n## ")
		if !found {
			t.Fatal("missing recovery section", heading)
		}
		for _, text := range []string{"five installation Secrets", "in no backup", "CA key", "forge deploy key", "SSH host key", "store TLS", "administrator", "storage-level copy", "data volume"} {
			if !strings.Contains(section, text) {
				t.Error(heading, "omits", text)
			}
		}
	}
	command := "(umask 077; kubectl --context \"$context\" -n \"$namespace\" get secret \\\n  oberth-ca oberth-forge-key oberth-ssh-host oberth-store-tls oberth-admin -o yaml \\\n  > /private/storage/oberth-secrets.yaml)"
	if !strings.Contains(guide, command) {
		t.Fatal("private five-Secret export missing")
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
