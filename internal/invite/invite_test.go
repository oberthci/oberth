package invite

import "testing"

func TestInviteURLRequiresDNS(t *testing.T) {
	for _, address := range []string{"https://localhost/mcp", "https://host:8443/mcp", "https://host.local/mcp"} {
		if err := ValidateURL(address); err != nil {
			t.Fatalf("DNS URL refused: %v", err)
		}
	}
	for _, address := range []string{"https://127.0.0.1/mcp", "https://[::1]/mcp", "https://[fe80::1%25lo0]/mcp", "http://localhost/mcp", "https://user@localhost/mcp"} {
		if err := ValidateURL(address); err == nil {
			t.Fatal("unsafe invite URL accepted")
		}
	}
}
