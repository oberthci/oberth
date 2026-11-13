package setuptui

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestWizardQuotesEveryShellValue(t *testing.T) {
	value := "space;$(fixture)\n'quote"
	state := &WizardState{Config: installer.Config{Namespace: value, ArgoNamespace: value, OpenBaoNamespace: value, NetworkPolicy: value, ArgoVaultCredentialedRole: value, TLSExtraDNSNames: []string{value}, TLSExtraIPs: []string{value}}, ClientAccess: value}
	line := BuildCommandLine(state)
	for _, flag := range []string{"namespace", "argo-namespace", "openbao-namespace", "network-policy", "argo-vault-credentialed-role", "tls-extra-dns-name", "tls-extra-ip", "client-access"} {
		if !strings.Contains(line, "--"+flag+"="+shellQuote(value)) {
			t.Errorf("unquoted %s in %s", flag, line)
		}
	}
}
