package api_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestMCPPlanPromisesOnlyTheSupportedWire(t *testing.T) {
	data, err := os.ReadFile("../../docs/plans/c1-transport-and-tool-registry.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	dates := regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`).FindAllString(text, -1)
	if len(dates) == 0 {
		t.Fatal("MCP plan omits its supported version")
	}
	for _, date := range dates {
		if date != apicontract.ProtocolVersion {
			t.Fatalf("MCP plan promises unsupported wire %s", date)
		}
	}
	for _, obsolete := range []string{"preferred date", "_meta?", "explicit negotiated dates"} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("MCP plan promises obsolete negotiation: %s", obsolete)
		}
	}
	for _, required := range []string{"Tool calls require `{name, arguments, _meta}`", "unsupported revisions are 400/-32022", "client identity is optional"} {
		if !strings.Contains(text, required) {
			t.Fatalf("MCP plan omits current contract: %s", required)
		}
	}
}
