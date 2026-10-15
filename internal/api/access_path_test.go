package api

import (
	"strings"
	"testing"
)

func TestAccessToolSchemasDescribeLiteralSecretPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"access_allow", "access_revoke"} {
		t.Run(name, func(t *testing.T) {
			for _, definition := range toolDefinitions() {
				if definition["name"] != name {
					continue
				}
				schema := definition["inputSchema"].(map[string]any)
				property := schema["properties"].(map[string]any)["secret"].(map[string]any)
				description := property["description"].(string)
				for _, required := range []string{"access_list", "oberth/data/release/cosign-secret", "no prefix expansion", "path normalization"} {
					if !strings.Contains(description, required) {
						t.Errorf("secret schema omits %q: %s", required, description)
					}
				}
				if strings.Contains(strings.ToLower(description), "short secret path") {
					t.Fatal("secret schema still advertises implicit short-path expansion")
				}
				return
			}
			t.Fatalf("missing tool %s", name)
		})
	}
}
