package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

// An access command missing a flag its grant kind needs, or given one it
// does not take, names that flag with the usage line and sends nothing.
func TestAccessArgumentErrorsNameTheFlag(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		problem, line string
	}{
		{[]string{"access", "revoke", "o/t/w", "--kind", "store_path", "--step", "private", "--secret", "walk/value"}, "missing --workflow for kind store_path", "oberth access revoke <repo> --workflow <file> --step <step> --secret <path>"},
		{[]string{"access", "revoke", "o/t/w", "--step", "private", "--secret", "walk/value", "--tier", "ci"}, "missing --workflow for kind store_path", "oberth access revoke <repo> --workflow <file> --step <step> --secret <path>"},
		{[]string{"access", "revoke", "o/t/w", "--workflow", ".oberth/build.yaml", "--step", "private", "--secret", "walk/value", "--tier", "ci"}, "--tier does not apply to kind store_path", "oberth access revoke <repo> --workflow <file> --step <step> --secret <path>"},
		{[]string{"access", "revoke", "o/t/w", "--kind", "pvc", "--claim", "data"}, "missing --tier for kind pvc", "oberth access revoke <repo> --kind pvc --claim <claim> --tier <ci|release>"},
		{[]string{"access", "revoke", "o/t/w", "--kind", "identity", "--workflow", ".oberth/build.yaml", "--step", "build", "--secret", "identity"}, "missing --audience for kind identity", "oberth access revoke <repo> --kind identity --workflow <file> --step <step> --secret <path> --audience <audience>"},
		{[]string{"access", "allow", "o/t/w", ".oberth/build.yaml"}, "missing --step for kind store_path", "oberth access allow --repo <repo> --workflow <file> --step <step> --secret <path>"},
		{[]string{"access", "allow", "--kind", "pvc", "o/t/w", "data"}, "missing --tier for kind pvc", "oberth access allow --kind pvc --repo <repo> --claim <claim> --tier <ci|release>"},
		{[]string{"access", "allow", "--tier", "ci", "o/t/w", ".oberth/build.yaml", "build", "key"}, "--tier does not apply to kind store_path", "oberth access allow --repo <repo> --workflow <file> --step <step> --secret <path>"},
		{[]string{"access", "dismiss", "--repo", "o/t/w", "--step", "build", "--secret", "key"}, "missing --workflow", ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			if code := app.run(t.Context(), tc.args); code != 2 || out.Len() != 0 {
				t.Fatal("argument error was not a usage refusal", code, out.String(), diagnostic.String())
			}
			text := diagnostic.String()
			if !strings.Contains(text, "\n"+tc.problem+"\n") || !strings.Contains(text, tc.line) {
				t.Fatalf("argument error does not name the flag and usage:\n%s", text)
			}
			for _, internal := range []string{"claim/tier", "field: grant", "Missing workflow"} {
				if strings.Contains(text, internal) {
					t.Fatal("argument error names an internal field", text)
				}
			}
		})
	}
}
