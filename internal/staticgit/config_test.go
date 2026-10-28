package staticgit

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestStaticComponentRecordsAndSBOMBindEachArchitecture(t *testing.T) {
	pins := Inputs()
	for _, key := range []string{"GIT_SOURCE_SHA256", "GIT_PATCH_SHA256", "GRYPE_AMD64_SHA256", "GRYPE_ARM64_SHA256"} {
		if value, err := hex.DecodeString(pins[key]); err != nil || len(value) != 32 {
			t.Fatalf("invalid %s", key)
		}
	}
	for _, pin := range strings.Fields(pins["APK_PACKAGES"]) {
		name, version, ok := strings.Cut(pin, "=")
		if !ok || name == "" || version == "" {
			t.Fatalf("unversioned package %q", pin)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		artifact, err := Qualified(arch)
		if err != nil {
			t.Fatal(err)
		}
		for _, hash := range []string{artifact.GitSHA256, artifact.MuslArchiveSHA256, artifact.ZlibArchiveSHA256} {
			if value, err := hex.DecodeString(hash); err != nil || len(value) != 32 {
				t.Fatalf("unbound %s artifact", arch)
			}
		}
		var output bytes.Buffer
		if err := WriteSBOM(&output, arch, false); err != nil {
			t.Fatal(err)
		}
		var document struct {
			Components []component `json:"components"`
		}
		if err := json.Unmarshal(output.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Components) != 3 {
			t.Fatalf("expected all three C components, got %+v", document)
		}
		components := map[string]component{}
		for _, component := range document.Components {
			components[component.Name] = component
		}
		if components["git"].Version != pins["GIT_VERSION"] || components["git"].CPE == "" || !strings.HasPrefix(components["git"].PURL, "pkg:generic/") {
			t.Fatal("source Git has no CPE or was falsely represented as an Alpine package")
		}
		if components["musl"].Version != pins["MUSL_VERSION"] || components["zlib"].Version != pins["ZLIB_VERSION"] {
			t.Fatal("linked library versions missing")
		}
		if err := ValidateBinary(arch, []byte("changed Git")); err == nil {
			t.Fatal("unqualified binary admitted")
		}
	}
	if _, err := Qualified("unknown"); err == nil {
		t.Fatal("unqualified platform admitted")
	}
}
