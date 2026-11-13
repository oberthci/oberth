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
