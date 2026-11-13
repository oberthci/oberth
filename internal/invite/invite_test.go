package invite

import "testing"

func TestInviteURLAcceptsDNSAndUnscopedIP(t *testing.T) {
	for _, address := range []string{"https://localhost/mcp", "https://host:8443/mcp", "https://host.local/mcp", "https://192.0.2.10:30443/mcp", "https://[2001:db8::10]:30443/mcp", "https://127.0.0.1/mcp", "https://[::1]/mcp"} {
		if err := ValidateURL(address); err != nil {
			t.Fatalf("client URL refused: %v", err)
		}
	}
	for _, address := range []string{"https://[fe80::1%25lo0]/mcp", "http://localhost/mcp", "https://user@localhost/mcp"} {
		if err := ValidateURL(address); err == nil {
			t.Fatal("unsafe invite URL accepted")
		}
	}
}
