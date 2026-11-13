package installer

import (
	"os"
	"strings"
	"testing"
)

func TestPluginSkillsKeepInstallerRecoveryPrivateAndDocumented(t *testing.T) {
	for _, skill := range []string{"install", "connect", "unseal", "add-repo", "oberth-agent-loop"} {
		raw, err := os.ReadFile(pluginPath(t, "skills", skill, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, rule := range []string{
			"When the installer fails or gives no clear reason, stay on the installer path",
			"read the public result JSON", "documented installer verify or resume command as printed",
			"stop\nand ask the user", "never invent a workaround outside the documented commands",
			"Never run administrative commands inside the server Pod (including upstream add\nor enrollment)",
			"directly or through another helper", "Do not switch to another skill to bypass this rule",
			"Never read, pipe or export the secret store's root token or any value from the\nsecrets file",
			"Never ask the user to paste a token\nor export BAO_TOKEN for the agent",
			"Report only verified facts", "never claim that nothing was pushed\nwithout checking evidence",
		} {
			if !strings.Contains(text, rule) {
				t.Errorf("%s lacks guardrail %q", skill, rule)
			}
		}
		if skill == "install" && strings.Contains(text, "run `upstream.next_command` exactly") {
			t.Fatal("install skill still directs in-Pod administration")
		}
	}
}

func TestInstallSkillKeepsOriginalCertificateDuringResume(t *testing.T) {
	raw, err := os.ReadFile(pluginPath(t, "skills", "install", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"Keep the original `--admin-csr`, `--admin-ssh-public-key`", "original CSR key and admin identity, without repeating enrollment", "never delete or replace the certificate to force enrollment", "stop if the original matching inputs are unavailable"} {
		if !strings.Contains(string(raw), rule) {
			t.Errorf("install skill lacks certificate recovery rule %q", rule)
		}
	}
}
