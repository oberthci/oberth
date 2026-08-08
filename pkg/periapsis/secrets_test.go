package periapsis

import (
	"slices"
	"strings"
	"testing"
)

func TestDeclaredReleaseSecretsReadsOnlyLiteralDeclaration(t *testing.T) {
	source := strings.Replace(representativeSource, "import \"oberth\"\n", `import "oberth"

var ReleaseSecrets = []string{"r2-upload-token", "cosign-secret"}
`, 1)
	got, err := DeclaredReleaseSecrets(source)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"r2-upload-token", "cosign-secret"}) {
		t.Fatalf("release Secrets = %v", got)
	}
}

func TestDeclaredSecretStoreSecretsReadsOnlySortedLiteralDeclaration(t *testing.T) {
	source := strings.Replace(representativeSource, "import \"oberth\"\n", `import "oberth"

var SecretStoreSecrets = map[string]string{
	"r2-token":  "ci/data/r2-upload",
	"gar-token": "ci/data/gar-push",
}
`, 1)
	got, err := DeclaredSecretStoreSecrets(source)
	if err != nil {
		t.Fatal(err)
	}
	want := []SecretStoreDeclaration{
		{Name: "gar-token", Path: "ci/data/gar-push"},
		{Name: "r2-token", Path: "ci/data/r2-upload"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("secret store secrets = %v, want %v", got, want)
	}
}

func TestDeclaredSecretStoreSecretsIsOptional(t *testing.T) {
	got, err := DeclaredSecretStoreSecrets(representativeSource)
	if err != nil || got != nil {
		t.Fatalf("absent declaration = %v, %v", got, err)
	}
}

func TestSecretStoreDeclarationInterpretsInsideTheJob(t *testing.T) {
	// The declaration is statically parsed on the server, but the Job-side
	// interpreter still evaluates the whole file; both must accept it.
	source := strings.Replace(representativeSource, "import \"oberth\"\n", `import "oberth"

var ReleaseSecrets = []string{"gar-sa-key"}

var SecretStoreSecrets = map[string]string{
	"r2-token": "ci/data/r2-upload",
}
`, 1)
	if _, err := Interpret(source, NewContext("demo", "main", "abc", "v1.0.0")); err != nil {
		t.Fatalf("declaration broke Job-side interpretation: %v", err)
	}
	declared, err := DeclaredSecretStoreSecrets(source)
	if err != nil || len(declared) != 1 {
		t.Fatalf("declared = %v, %v", declared, err)
	}
}

func TestDeclaredSecretStoreSecretsRequiresExplicitStaticContract(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		want        string
	}{
		{name: "computed", declaration: `var SecretStoreSecrets = requestedSecrets()`, want: "map[string]string literal"},
		{name: "wrong type", declaration: `var SecretStoreSecrets = []string{"ci/data/r2-upload"}`, want: "map[string]string literal"},
		{name: "computed key", declaration: `var SecretStoreSecrets = map[string]string{secretName: "ci/data/r2-upload"}`, want: "string literal"},
		{name: "computed value", declaration: `var SecretStoreSecrets = map[string]string{"r2-token": secretPath()}`, want: "string literal"},
		{name: "duplicate name", declaration: `var SecretStoreSecrets = map[string]string{"r2-token": "ci/data/a", "r2-token": "ci/data/b"}`, want: "duplicate name"},
		{name: "duplicate path", declaration: `var SecretStoreSecrets = map[string]string{"a": "ci/data/r2-upload", "b": "ci/data/r2-upload"}`, want: "duplicate path"},
		{name: "empty path", declaration: `var SecretStoreSecrets = map[string]string{"r2-token": ""}`, want: "is invalid"},
		{name: "traversal path", declaration: `var SecretStoreSecrets = map[string]string{"r2-token": "ci/../sys/raw"}`, want: "clean non-empty segments"},
		{name: "reserved backend", declaration: `var SecretStoreSecrets = map[string]string{"r2-token": "sys/raw/ci"}`, want: "reserved"},
		{name: "path charset", declaration: `var SecretStoreSecrets = map[string]string{"r2-token": "ci/data/r2 upload"}`, want: "only ASCII"},
		{name: "empty explicit", declaration: `var SecretStoreSecrets = map[string]string{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(representativeSource, "import \"oberth\"\n", "import \"oberth\"\n\n"+test.declaration+"\n", 1)
			got, err := DeclaredSecretStoreSecrets(source)
			if test.want == "" {
				if err != nil || len(got) != 0 {
					t.Fatalf("empty declaration = %v, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateSecretStorePath(t *testing.T) {
	valid := []string{"ci/data/r2-upload", "secret/data/nested/deep.name", "kv_v2/data/a-b_c.d"}
	for _, path := range valid {
		if err := ValidateSecretStorePath(path); err != nil {
			t.Fatalf("ValidateSecretStorePath(%q) = %v", path, err)
		}
	}
	invalid := map[string]string{
		"":                       "1-512 bytes",
		"/ci/data/x":             "clean non-empty segments",
		"ci/data/x/":             "clean non-empty segments",
		"ci//data":               "clean non-empty segments",
		"ci/./data":              "clean non-empty segments",
		"ci/../data":             "clean non-empty segments",
		"auth/kubernetes/role":   "reserved",
		"sys/raw":                "reserved",
		"identity/entity":        "reserved",
		"cubbyhole/x":            "reserved",
		"ci/data/x y":            "only ASCII",
		"ci/data/x\n":            "only ASCII",
		strings.Repeat("a", 513): "1-512 bytes",
	}
	for path, want := range invalid {
		if err := ValidateSecretStorePath(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ValidateSecretStorePath(%q) = %v, want %q", path, err, want)
		}
	}
}

func TestDeclaredReleaseSecretsRequiresExplicitStaticContract(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		want        string
	}{
		{name: "missing", want: "must declare release credentials"},
		{name: "computed", declaration: `var ReleaseSecrets = requestedSecrets()`, want: "[]string literal"},
		{name: "duplicate", declaration: `var ReleaseSecrets = []string{"cosign-secret", "cosign-secret"}`, want: "duplicate"},
		{name: "nonliteral entry", declaration: `var ReleaseSecrets = []string{secretName}`, want: "string literal"},
		{name: "empty explicit", declaration: `var ReleaseSecrets = []string{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(representativeSource, "import \"oberth\"\n", "import \"oberth\"\n\n"+test.declaration+"\n", 1)
			got, err := DeclaredReleaseSecrets(source)
			if test.want == "" {
				if err != nil || len(got) != 0 {
					t.Fatalf("empty declaration = %v, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
