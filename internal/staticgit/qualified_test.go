package staticgit

import (
	"debug/elf"
	"encoding/json"
	"strings"
	"testing"
)

func installedPackages() string {
	var body strings.Builder
	for _, pin := range strings.Fields(Inputs()["APK_PACKAGES"]) {
		name, version, _ := strings.Cut(pin, "=")
		body.WriteString("P:" + name + "\nV:" + version + "\n\n")
	}
	return body.String()
}

func TestReviewedRecordsAndPackageSBOMGoldens(t *testing.T) {
	for _, tc := range []struct {
		arch     Architecture
		platform string
	}{{AMD64, "x86_64"}, {ARM64, "aarch64"}} {
		r, err := Qualified(tc.arch)
		if err != nil {
			t.Fatal(err)
		}
		body, err := r.SBOM()
		if err != nil {
			t.Fatal(err)
		}
		var bom struct {
			Components []component `json:"components"`
		}
		if err := json.Unmarshal(body, &bom); err != nil {
			t.Fatal(err)
		}
		for _, want := range []struct {
			index        int
			purl, linked string
		}{
			{1, "pkg:apk/alpine/musl@1.2.5-r24?arch=" + tc.platform + "&distro=alpine-3.23.8", "musl-dev"},
			{2, "pkg:apk/alpine/zlib@1.4-r0?arch=" + tc.platform + "&distro=alpine-3.23.8", "zlib-static"},
		} {
			got := bom.Components[want.index]
			if got.PURL != want.purl || len(got.Properties) != 1 || got.Properties[0] != (property{"oberth:linked-from-apk", want.linked}) {
				t.Fatalf("component = %+v; want %s from %s", got, want.purl, want.linked)
			}
		}
	}
	if _, err := Qualified("unknown"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
	copy := Inputs()
	copy["GIT_VERSION"] = "changed"
	if Inputs()["GIT_VERSION"] != "2.56.0" {
		t.Fatal("reviewed data is mutable")
	}
}

func TestVerifyBuildRequiresEveryReviewedPackagePin(t *testing.T) {
	body := executable(elf.EM_X86_64)
	r := record(body)
	for _, bad := range []string{
		"", strings.Replace(installedPackages(), "V:1.2.5-r24", "V:changed", 1),
		strings.Replace(installedPackages(), "P:musl\n", "P:missing\n", 1),
	} {
		if r.VerifyBuild(body, []byte("musl"), []byte("zlib"), bad) == nil {
			t.Fatal("builder package drift accepted")
		}
	}
	if err := r.VerifyBuild(body, []byte("musl"), []byte("zlib"), installedPackages()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBuildAllowsUnpinnedInstalledPackages(t *testing.T) {
	body := executable(elf.EM_X86_64)
	for _, extra := range []string{"P:unexpected\nV:1\n\n", "P:unexpected\n\n", "ignored metadata\n\n"} {
		if err := record(body).VerifyBuild(body, []byte("musl"), []byte("zlib"), installedPackages()+extra); err != nil {
			t.Fatal(err)
		}
	}
}
