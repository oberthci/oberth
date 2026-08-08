package runner

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const cosignFixturePayload = "#!/bin/sh\nprintf 'GitVersion:    v3.1.1\\n'\n"

func TestRepositoryOwnsPinnedToolSetup(t *testing.T) {
	t.Parallel()
	installerPath := "../../.oberth/install-tools.sh"
	body, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("repository tool installer is not executable")
	}
	installer := string(body)
	for name, required := range map[string]string{
		"Go archive":                   "https://go.dev/dl/go1.26.5.linux-amd64.tar.gz",
		"Go checksum":                  "5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053",
		"Python archive":               "https://github.com/astral-sh/python-build-standalone/releases/download/20260718/cpython-3.13.14%2B20260718-x86_64-unknown-linux-musl-install_only_stripped.tar.gz",
		"Python version":               "version=3.13.14",
		"Python build":                 "build=20260718",
		"Python checksum":              "0dbfe4b9c043f81a60953245a2735594f6b09308031086ceac15511f4c0af193",
		"Python binary":                "c524653b6cc492851f59bf5572c81f3f9b518fa84b34e734c59ba06848d65a8f",
		"fetch timeout":                "--connect-timeout 10 --max-time 90",
		"retry bound":                  "--retry 2 --retry-all-errors --retry-max-time 180",
		"HTTPS redirects":              "--proto '=https' --proto-redir '=https' --tlsv1.2",
		"observable fetch":             "--progress-bar --show-error",
		"observable filter":            `busybox awk 'BEGIN { RS = "\r" } { print; fflush() }'`,
		"scoped status":                "curl_status_file=$partial.status",
		"bounded cache copy":           `busybox head -c "$bounded_size"`,
		"descriptor cache identity":    `busybox stat -Lc "%d:%i" /proc/self/fd/3`,
		"bounded cache open":           `busybox timeout 10 busybox sh -c`,
		"fixed cache stage":            `cache_partial=$download_cache/.oberth-download-$slot.stage`,
		"fixed cache lock":             `cache_lock=$download_cache/.oberth-download-$1.lock`,
		"cache lock owner identity":    `[ "$current_lock_identity" != "$cache_lock_identity" ]`,
		"no-target cache publication":  `mv -fT "$cache_partial"`,
		"Go size":                      `"66879095"`,
		"Python size":                  `"27710872"`,
		"Zig size":                     `"49086504"`,
		"xz source":                    "github.com/ulikunitz/xz@v0.5.16",
		"xz sum":                       "h1:ld6NyySjx5lowVKwJvMRLnW5nxKX/xnpSiFYZ/Lxur0=",
		"xz mod sum":                   "h1:H9Rt/W6/Qj27PGauhQc6nfCDy7vHpzsOThBSaYDoEhw=",
		"xz binary":                    "ac4688de237077cbade4c16cfc2ab32f22ba2ff4ffb8bc1ad39ade32dd09e696",
		"Zig archive":                  "https://ziglang.org/download/0.14.1/zig-x86_64-linux-0.14.1.tar.xz",
		"Zig checksum":                 "24aeeec8af16c381934a6cd7d95c807a8cb2cf7df9fa40d359aa884195c4716c",
		"Cosign size":                  `"140588378"`,
		"golangci archive":             "golangci-lint-2.12.2-oberth.1-linux-amd64.gz",
		"golangci archive checksum":    "5675ce9cba0d39c751a6c4302b26d79a38cc4904e8eba586395b70796acb16a0",
		"golangci archive size":        `"14783489"`,
		"golangci binary size":         `"40685694"`,
		"golangci binary":              "30d5e2e3715aed6e3a1008c9e074e50291926213d1b4fc50fa27e08ccaed7438",
		"golangci patched dependency":  "dep[[:space:]]+golang.org/x/text[[:space:]]+v0.39.0",
		"golangci derivative identity": "version 2.12.2+oberth.1",
		"Trivy archive":                "trivy-0.73.0-oberth.1-linux-amd64.gz",
		"Trivy archive checksum":       "9faca4cd2dc9a1acd72e195ff1f5fed084b5cb0b11086aa3b2ffca7420e6c029",
		"Trivy archive size":           `"48757831"`,
		"Trivy binary size":            `"166244478"`,
		"Trivy binary":                 "cce775a873e9df2468a5578148907380cf1f07b4c27ecec25d9b1da10dec0406",
		"Trivy patched dependency":     "dep[[:space:]]+oras.land/oras-go/v2[[:space:]]+v2.6.2",
		"Trivy derivative identity":    "Version: 0.73.0+oberth.1",
		"Helm archive":                 "helm-4.2.3-oberth.1-linux-amd64.gz",
		"Helm archive checksum":        "e04e9b95e7a8377354b74f727033731ee095e8316401c8ac2423b058e614b1bd",
		"Helm archive size":            `"19653291"`,
		"Helm binary size":             `"64270462"`,
		"Helm binary":                  "ae8c0c2bcd766239f8ef834e622449a1d1261a33e63e63287dfb4f9dc4072cd9",
		"Helm derivative identity":     "v4.2.3+oberth.1.g43e8b7f",
		"gzip extraction":              `busybox gzip -dc "$archive"`,
		"writable install":             "OBERTH_TOOLS_DIR",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("repository tool installer lacks %s pin %q", name, required)
		}
	}
	for _, forbidden := range []string{"/usr/local/bin", "/seed", "apk add", "sudo ", "GOTOOLCHAIN=auto"} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("repository tool installer retains image-owned behavior %q", forbidden)
		}
	}
	if strings.Contains(installer, `cache_partial=$download_cache/.$name.$$.partial`) {
		t.Fatal("shared download-cache publication reuses container-local PID names")
	}
	if strings.Contains(installer, `mktemp "$download_cache/`) {
		t.Fatal("persistent download-cache publication can accumulate crash-left random paths")
	}
	if strings.Contains(installer, "lock_age") {
		t.Fatal("repository-controlled Jobs attempt an unsafe stale-lock pathname reclaim")
	}
	slotCaseStart := strings.Index(installer, "case \"$slot\" in")
	if slotCaseStart < 0 {
		t.Fatal("download-cache slot allowlist is absent")
	}
	slotCase := installer[slotCaseStart+len("case \"$slot\" in"):]
	slotCaseEnd := strings.Index(slotCase, "*) fail")
	if slotCaseEnd < 0 {
		t.Fatal("download-cache slot allowlist has no fail-closed default")
	}
	if strings.TrimSpace(slotCase[:slotCaseEnd]) != "go|python|zig|golangci|trivy|helm|cosign) ;;" {
		t.Fatalf("download-cache slot allowlist is not exactly seven fixed slots: %q", slotCase[:slotCaseEnd])
	}
	if got := strings.Count(installer, "fetch_verified \\"); got != 7 {
		t.Fatalf("repository installer has %d verified-download call sites, want exactly seven", got)
	}
	for size, slot := range map[string]string{
		"66879095": "go", "27710872": "python", "49086504": "zig",
		"14783489": "golangci", "48757831": "trivy", "19653291": "helm",
		"140588378": "cosign",
	} {
		start := strings.Index(installer, `"`+size+`"`)
		if start < 0 {
			t.Fatalf("download size %s is absent", size)
		}
		callTail := installer[start:]
		if next := strings.Index(callTail[1:], "fetch_verified \\"); next >= 0 {
			callTail = callTail[:next+1]
		}
		if !strings.Contains(callTail, fmt.Sprintf("%q)", slot)) {
			t.Fatalf("download size %s is not mapped one-to-one to cache slot %q", size, slot)
		}
	}

	pipelineBody, err := os.ReadFile("../../.oberth/periapsis.go")
	if err != nil {
		t.Fatal(err)
	}
	pipeline := string(pipelineBody)
	for _, required := range []string{
		`Retrograde("setup",`,
		`Step{Name: "install-go-python", Command: "./.oberth/install-tools.sh", Args: []string{"go", "python"}}`,
		`Step{Name: "install-zig", Command: "./.oberth/install-tools.sh", Args: []string{"zig"}}`,
		`Step{Name: "install-golangci", Command: "./.oberth/install-tools.sh", Args: []string{"golangci"}}`,
		`Step{Name: "install-trivy", Command: "./.oberth/install-tools.sh", Args: []string{"trivy"}}`,
		`Step{Name: "install-helm", Command: "./.oberth/install-tools.sh", Args: []string{"helm"}}`,
		`Retrograde("lint",`,
		`).DependsOn("setup").`,
		`Retrograde("security",`,
		`).DependsOn("setup").`,
	} {
		if !strings.Contains(pipeline, required) {
			t.Fatalf("Oberth periapsis lacks explicit setup contract %q", required)
		}
	}
	if got := strings.Count(pipeline, `Step{Name: "install-go-python", Command: "./.oberth/install-tools.sh", Args: []string{"go", "python"}}`); got != 2 {
		t.Fatalf("Oberth periapsis declares %d install-go-python steps, want CI and release setup", got)
	}
	if !strings.Contains(installer, "install_zig() {\n  install_xz\n") {
		t.Fatal("Zig setup does not bootstrap the pinned repository-owned xz tool")
	}
	if strings.Contains(installer, "tar -xJ") || !strings.Contains(installer, `"$bin_dir/xz" -d -c "$archive" >"$decompressed"`) {
		t.Fatal("Zig extraction does not invoke the verified repository-owned xz executable explicitly")
	}
	if strings.Contains(installer, "prepare_module_source golangci-lint") ||
		strings.Contains(installer, "prepare_module_source trivy") ||
		strings.Contains(installer, "prepare_module_source helm") {
		t.Fatal("large patched CI tools are still rebuilt from source in each Job")
	}

	provenanceBody, err := os.ReadFile("../../.oberth/toolchain-artifacts.json")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(provenanceBody) {
		t.Fatal("toolchain artifact provenance is not valid JSON")
	}
	type artifactRecord struct {
		Name   string `json:"name"`
		Source struct {
			Module            string `json:"module"`
			Sum               string `json:"sum"`
			GoModSum          string `json:"goModSum"`
			PatchedDependency string `json:"patchedDependency"`
		} `json:"source"`
		Build struct {
			Environment string `json:"environment"`
			Package     string `json:"package"`
			Flags       string `json:"flags"`
			LDFlags     string `json:"ldflags"`
		} `json:"build"`
		Archive struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"archive"`
		Binary struct {
			SHA256   string `json:"sha256"`
			Size     int64  `json:"size"`
			Identity string `json:"identity"`
		} `json:"binary"`
	}
	var provenance struct {
		Schema      string `json:"schema"`
		Status      string `json:"status"`
		PublishedAt string `json:"publishedAt"`
		Origin      string `json:"origin"`
		Generator   struct {
			GoVersion   string `json:"goVersion"`
			Compression string `json:"compression"`
		} `json:"generator"`
		Artifacts []artifactRecord `json:"artifacts"`
	}
	if err := json.Unmarshal(provenanceBody, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Schema != "oberth.toolchain-artifacts.v1" ||
		provenance.Status != "published-and-read-back" ||
		provenance.PublishedAt != "2026-08-07T06:52:40Z" ||
		provenance.Origin != "https://releases.cloudtaser.io/oberth/toolchains/" ||
		provenance.Generator.GoVersion != "go1.26.5" ||
		provenance.Generator.Compression != "gzip -n -9" {
		t.Fatalf("toolchain provenance header is incomplete: %+v", provenance)
	}
	type expectedArtifact struct {
		module, sum, modSum, patch                  string
		buildEnvironment, buildPackage, ldflags     string
		archiveURL, archiveSHA, binarySHA, identity string
		archiveSize, binarySize                     int64
	}
	expectedArtifacts := map[string]expectedArtifact{
		"golangci-lint": {
			module: "github.com/golangci/golangci-lint/v2@v2.12.2", sum: "h1:7+d1uY0bq1MU2UV3R5pW5Q7QWdcoq4naMRXM+gsJKrs=", modSum: "h1:opqHHuIcTG2R+4akzWMd4o1BnD9/1LcjICWOujr91U8=", patch: "golang.org/x/text@v0.39.0",
			buildEnvironment: "CGO_ENABLED=0 GOOS=linux GOARCH=amd64", buildPackage: "./cmd/golangci-lint", ldflags: "-s -w -buildid= -X main.version=2.12.2+oberth.1 -X main.commit=c0d3ddc9cf3faa61a4e378e879ece580256d76e5 -X main.date=2026-08-05T00:00:00Z",
			archiveURL: "https://releases.cloudtaser.io/oberth/toolchains/sha256-5675ce9cba0d39c751a6c4302b26d79a38cc4904e8eba586395b70796acb16a0/golangci-lint-2.12.2-oberth.1-linux-amd64.gz", archiveSHA: "5675ce9cba0d39c751a6c4302b26d79a38cc4904e8eba586395b70796acb16a0", archiveSize: 14783489,
			binarySHA: "30d5e2e3715aed6e3a1008c9e074e50291926213d1b4fc50fa27e08ccaed7438", binarySize: 40685694, identity: "golangci-lint 2.12.2+oberth.1",
		},
		"trivy": {
			module: "github.com/aquasecurity/trivy@v0.73.0", sum: "h1:KOhSU8XlhYiHpYlOVFmue/akNU+adLb5gXDtrlmuwqs=", modSum: "h1:rvWBubBeHD4aYtjlHH2NJ/eeFSnqEpAJGOd9+syR6Z4=", patch: "oras.land/oras-go/v2@v2.6.2",
			buildEnvironment: "GOEXPERIMENT=jsonv2 CGO_ENABLED=0 GOOS=linux GOARCH=amd64", buildPackage: "./cmd/trivy", ldflags: "-s -w -buildid= -X github.com/aquasecurity/trivy/pkg/version/app.ver=0.73.0+oberth.1",
			archiveURL: "https://releases.cloudtaser.io/oberth/toolchains/sha256-9faca4cd2dc9a1acd72e195ff1f5fed084b5cb0b11086aa3b2ffca7420e6c029/trivy-0.73.0-oberth.1-linux-amd64.gz", archiveSHA: "9faca4cd2dc9a1acd72e195ff1f5fed084b5cb0b11086aa3b2ffca7420e6c029", archiveSize: 48757831,
			binarySHA: "cce775a873e9df2468a5578148907380cf1f07b4c27ecec25d9b1da10dec0406", binarySize: 166244478, identity: "trivy 0.73.0+oberth.1",
		},
		"helm": {
			module: "helm.sh/helm/v4@v4.2.3", sum: "h1:JEejtPE04+SvyRomOfgRXVxyJ/lude7eShio30oQr0Y=", modSum: "h1:azI2XpxowOGXAgzeXcqyfskUmIfILqIcJxiFw1M6PuM=", patch: "oras.land/oras-go/v2@v2.6.2",
			buildEnvironment: "CGO_ENABLED=0 GOOS=linux GOARCH=amd64", buildPackage: "./cmd/helm", ldflags: "-s -w -buildid= -X helm.sh/helm/v4/internal/version.version=v4.2.3 -X helm.sh/helm/v4/internal/version.metadata=oberth.1.g43e8b7f -X helm.sh/helm/v4/internal/version.gitTreeState=patched",
			archiveURL: "https://releases.cloudtaser.io/oberth/toolchains/sha256-e04e9b95e7a8377354b74f727033731ee095e8316401c8ac2423b058e614b1bd/helm-4.2.3-oberth.1-linux-amd64.gz", archiveSHA: "e04e9b95e7a8377354b74f727033731ee095e8316401c8ac2423b058e614b1bd", archiveSize: 19653291,
			binarySHA: "ae8c0c2bcd766239f8ef834e622449a1d1261a33e63e63287dfb4f9dc4072cd9", binarySize: 64270462, identity: "helm v4.2.3+oberth.1.g43e8b7f",
		},
	}
	if len(provenance.Artifacts) != len(expectedArtifacts) {
		t.Fatalf("toolchain provenance contains %d artifacts, want %d", len(provenance.Artifacts), len(expectedArtifacts))
	}
	for _, artifact := range provenance.Artifacts {
		expected, ok := expectedArtifacts[artifact.Name]
		if !ok {
			t.Fatalf("toolchain provenance contains unexpected artifact %q", artifact.Name)
		}
		if artifact.Source.Module != expected.module || artifact.Source.Sum != expected.sum ||
			artifact.Source.GoModSum != expected.modSum || artifact.Source.PatchedDependency != expected.patch ||
			artifact.Build.Environment != expected.buildEnvironment || artifact.Build.Package != expected.buildPackage ||
			artifact.Build.Flags != "-trimpath -buildvcs=false" || artifact.Build.LDFlags != expected.ldflags ||
			artifact.Archive.URL != expected.archiveURL || artifact.Archive.SHA256 != expected.archiveSHA ||
			artifact.Archive.Size != expected.archiveSize || artifact.Binary.SHA256 != expected.binarySHA ||
			artifact.Binary.Size != expected.binarySize || artifact.Binary.Identity != expected.identity {
			t.Fatalf("toolchain provenance record for %s is incomplete: %+v", artifact.Name, artifact)
		}
		delete(expectedArtifacts, artifact.Name)
	}
	if len(expectedArtifacts) != 0 {
		t.Fatalf("toolchain provenance omits artifacts: %v", expectedArtifacts)
	}
}

type compressedToolContract struct {
	name, installArgument, binaryName                      string
	archiveHashPin, archiveSizePin, binaryHashPin, sizePin string
	dependencyModule, dependencyVersion                    string
	dependencyFailure, identityFailure                     string
	payload                                                []byte
}

type compressedToolScenario struct {
	name                string
	invalidArchive      bool
	binarySizeDelta     int
	wrongBinaryHash     bool
	badDependency       bool
	badIdentity         bool
	wantFailure         string
	wantPublished       bool
	wantDependencyProbe bool
	wantIdentityProbe   bool
}

func TestCompressedToolArchiveInstallationIsVerifiedAndAtomic(t *testing.T) {
	t.Parallel()
	tools := []compressedToolContract{
		{
			name: "golangci-lint", installArgument: "golangci", binaryName: "golangci-lint",
			archiveHashPin: "5675ce9cba0d39c751a6c4302b26d79a38cc4904e8eba586395b70796acb16a0", archiveSizePin: "14783489",
			binaryHashPin: "30d5e2e3715aed6e3a1008c9e074e50291926213d1b4fc50fa27e08ccaed7438", sizePin: "40685694",
			dependencyModule: "golang.org/x/text", dependencyVersion: "v0.39.0",
			dependencyFailure: "golangci-lint lacks the patched x/text dependency", identityFailure: "golangci-lint derivative identity is wrong",
			payload: []byte(`#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = version ] || exit 91
: >"$OBERTH_TOOL_IDENTITY_MARKER"
if [ "${OBERTH_BAD_IDENTITY:-0}" -eq 1 ]; then
  printf 'wrong identity\n'
else
  printf 'golangci-lint has version 2.12.2+oberth.1\n'
fi
`),
		},
		{
			name: "trivy", installArgument: "trivy", binaryName: "trivy",
			archiveHashPin: "9faca4cd2dc9a1acd72e195ff1f5fed084b5cb0b11086aa3b2ffca7420e6c029", archiveSizePin: "48757831",
			binaryHashPin: "cce775a873e9df2468a5578148907380cf1f07b4c27ecec25d9b1da10dec0406", sizePin: "166244478",
			dependencyModule: "oras.land/oras-go/v2", dependencyVersion: "v2.6.2",
			dependencyFailure: "Trivy lacks the patched oras dependency", identityFailure: "Trivy derivative identity is wrong",
			payload: []byte(`#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = --version ] || exit 91
: >"$OBERTH_TOOL_IDENTITY_MARKER"
if [ "${OBERTH_BAD_IDENTITY:-0}" -eq 1 ]; then
  printf 'wrong identity\n'
else
  printf 'Version: 0.73.0+oberth.1\n'
fi
`),
		},
		{
			name: "helm", installArgument: "helm", binaryName: "helm",
			archiveHashPin: "e04e9b95e7a8377354b74f727033731ee095e8316401c8ac2423b058e614b1bd", archiveSizePin: "19653291",
			binaryHashPin: "ae8c0c2bcd766239f8ef834e622449a1d1261a33e63e63287dfb4f9dc4072cd9", sizePin: "64270462",
			dependencyModule: "oras.land/oras-go/v2", dependencyVersion: "v2.6.2",
			dependencyFailure: "Helm lacks the patched oras dependency", identityFailure: "Helm derivative identity is wrong",
			payload: []byte(`#!/bin/sh
[ "$#" -eq 2 ] && [ "$1" = version ] && [ "$2" = --short ] || exit 91
: >"$OBERTH_TOOL_IDENTITY_MARKER"
if [ "${OBERTH_BAD_IDENTITY:-0}" -eq 1 ]; then
  printf 'wrong identity\n'
else
  printf 'v4.2.3+oberth.1.g43e8b7f\n'
fi
`),
		},
	}
	for _, tool := range tools {
		tool := tool
		t.Run(tool.name, func(t *testing.T) {
			for _, test := range []compressedToolScenario{
				{name: "success", wantPublished: true, wantDependencyProbe: true, wantIdentityProbe: true},
				{name: "invalid gzip", invalidArchive: true, wantFailure: "archive extraction failed"},
				{name: "wrong expanded size", binarySizeDelta: 1, wantFailure: "size mismatch"},
				{name: "wrong expanded checksum", wrongBinaryHash: true, wantFailure: "checksum mismatch"},
				{name: "wrong patched dependency", badDependency: true, wantFailure: tool.dependencyFailure, wantPublished: true, wantDependencyProbe: true},
				{name: "wrong derivative identity", badIdentity: true, wantFailure: tool.identityFailure, wantPublished: true, wantDependencyProbe: true, wantIdentityProbe: true},
			} {
				test := test
				t.Run(test.name, func(t *testing.T) {
					runCompressedToolScenario(t, tool, test)
				})
			}
		})
	}
}

func runCompressedToolScenario(t *testing.T, tool compressedToolContract, test compressedToolScenario) {
	t.Helper()
	archive := gzipBytes(t, tool.payload)
	if test.invalidArchive {
		archive = []byte("not a gzip stream")
	}
	binaryHash := digestHex(tool.payload)
	if test.wrongBinaryHash {
		binaryHash = strings.Repeat("0", 64)
	}
	toolsDir, err := os.MkdirTemp("/tmp", "oberth-tools-gzip-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolsDir) })
	stubDir := t.TempDir()
	archivePath := filepath.Join(toolsDir, "fixture.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(toolsDir, "go", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, filepath.Join(toolsDir, "go", "bin", "go"), `#!/bin/sh
[ "$#" -eq 3 ] && [ "$1" = version ] && [ "$2" = -m ] || exit 91
[ "$3" = "$OBERTH_EXPECTED_TOOL_PATH" ] || exit 92
: >"$OBERTH_GO_VERSION_MARKER"
if [ "${OBERTH_BAD_DEPENDENCY:-0}" -eq 1 ]; then
  printf 'dep\texample.invalid/dependency\tv0.0.0\n'
else
  printf 'dep\t%s\t%s\n' "$OBERTH_EXPECTED_DEPENDENCY_MODULE" "$OBERTH_EXPECTED_DEPENDENCY_VERSION"
fi
`)
	writeTestExecutable(t, filepath.Join(stubDir, "curl"), `#!/bin/sh
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output" ]; then
    shift
    output=$1
  fi
  shift
done
[ -n "$output" ] || exit 90
cp "$OBERTH_COMPRESSED_TOOL_ARCHIVE" "$output"
`)
	installerBody, err := os.ReadFile("../../.oberth/install-tools.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBody)
	installer = strings.ReplaceAll(installer, tool.archiveHashPin, digestHex(archive))
	installer = strings.ReplaceAll(installer, `"`+tool.archiveSizePin+`"`, `"`+strconv.Itoa(len(archive))+`"`)
	installer = strings.ReplaceAll(installer, tool.binaryHashPin, binaryHash)
	installer = strings.ReplaceAll(installer, `"`+tool.sizePin+`"`, `"`+strconv.Itoa(len(tool.payload)+test.binarySizeDelta)+`"`)
	installerPath := filepath.Join(toolsDir, "install-tools.sh")
	if err := os.WriteFile(installerPath, []byte(installer), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(toolsDir, "bin", tool.binaryName)
	dependencyMarker := filepath.Join(toolsDir, "dependency-probed")
	identityMarker := filepath.Join(toolsDir, "identity-probed")
	badDependency := "0"
	if test.badDependency {
		badDependency = "1"
	}
	badIdentity := "0"
	if test.badIdentity {
		badIdentity = "1"
	}
	command := exec.Command("busybox", "sh", installerPath, tool.installArgument)
	command.Env = append(os.Environ(),
		"OBERTH_TOOLS_DIR="+toolsDir,
		"OBERTH_COMPRESSED_TOOL_ARCHIVE="+archivePath,
		"OBERTH_EXPECTED_TOOL_PATH="+target,
		"OBERTH_EXPECTED_DEPENDENCY_MODULE="+tool.dependencyModule,
		"OBERTH_EXPECTED_DEPENDENCY_VERSION="+tool.dependencyVersion,
		"OBERTH_GO_VERSION_MARKER="+dependencyMarker,
		"OBERTH_TOOL_IDENTITY_MARKER="+identityMarker,
		"OBERTH_BAD_DEPENDENCY="+badDependency,
		"OBERTH_BAD_IDENTITY="+badIdentity,
		"PATH="+stubDir+":"+os.Getenv("PATH"),
	)
	output, commandErr := command.CombinedOutput()
	if test.wantFailure == "" {
		if commandErr != nil {
			t.Fatalf("install compressed %s: %v\n%s", tool.name, commandErr, output)
		}
	} else {
		if commandErr == nil {
			t.Fatalf("compressed %s setup accepted %s failure:\n%s", tool.name, test.name, output)
		}
		if !strings.Contains(string(output), test.wantFailure) {
			t.Fatalf("%s %s failure omitted %q:\n%s", tool.name, test.name, test.wantFailure, output)
		}
	}
	if test.wantPublished {
		installed, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(installed, tool.payload) {
			t.Fatal("installed compressed-tool payload differs from the verified input")
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("installed compressed-tool mode = %o, want 755", info.Mode().Perm())
		}
	} else if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("failed compressed tool was published: %v", err)
	}
	assertMarkerState(t, dependencyMarker, test.wantDependencyProbe)
	assertMarkerState(t, identityMarker, test.wantIdentityProbe)
	staged, err := filepath.Glob(filepath.Join(toolsDir, "bin", "."+tool.binaryName+".*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("compressed tool setup left staging files: %v", staged)
	}
}

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	compressor := gzip.NewWriter(&compressed)
	if _, err := compressor.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func assertMarkerState(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil {
		t.Fatalf("expected probe marker %s: %v", path, err)
	}
	if !want && !os.IsNotExist(err) {
		t.Fatalf("unexpected probe marker %s: %v", path, err)
	}
}

func digestHex(payload []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

func TestRepositoryInstallerRequestsObservableVerifiedFetch(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	command := fixture.command()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install cosign with stubbed verified fetch: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("fetch-progress-marker\n")) {
		t.Fatalf("installer did not expose curl progress on stderr: %q", output)
	}
	rawArgs, err := os.ReadFile(fixture.argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(rawArgs)), "\n")
	for _, required := range []string{
		"--fail", "--location", "--progress-bar", "--show-error",
		"--connect-timeout", "10", "--max-time", "90",
		"--retry", "2", "--retry-all-errors", "--retry-max-time", "180",
		"--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "--output",
	} {
		if !slices.Contains(args, required) {
			t.Fatalf("curl arguments %q lack %q", args, required)
		}
	}
	if slices.Contains(args, "--silent") {
		t.Fatalf("curl arguments suppress progress: %q", args)
	}
}

func TestRepositoryInstallerProgressReachesNewlineOnlyPredecessorBeforeFetchExit(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	command := fixture.command("OBERTH_CURL_WAIT=" + fixture.releasePath)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	line := make(chan string, 1)
	go func() {
		value, _ := bufio.NewReader(stderr).ReadString('\n')
		line <- value
	}()

	var observationFailure string
	commandFinished := false
	select {
	case value := <-line:
		if value != "\n" {
			observationFailure = fmt.Sprintf("leading curl carriage-return heartbeat = %q, want newline", value)
		}
	case err := <-finished:
		commandFinished = true
		observationFailure = fmt.Sprintf("installer exited before progress was observable: %v", err)
	case <-time.After(2 * time.Second):
		observationFailure = "newline-only predecessor did not receive progress before fetch completion"
	}
	if observationFailure == "" {
		select {
		case err := <-finished:
			commandFinished = true
			observationFailure = fmt.Sprintf("installer exited before the blocked fetch was released: %v", err)
		default:
		}
	}
	if err := os.WriteFile(fixture.releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !commandFinished {
		if err := <-finished; err != nil && observationFailure == "" {
			t.Fatalf("installer failed after releasing fetch: %v", err)
		}
	}
	if observationFailure != "" {
		t.Fatal(observationFailure)
	}
}

func TestRepositoryInstallerPreservesCurlFailureStatusThroughProgressFilter(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	output, err := fixture.command("OBERTH_CURL_EXIT=37").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 37 {
		t.Fatalf("installer error = %v, want curl exit 37; output=%q", err, output)
	}
	if !bytes.Contains(output, []byte("fetch-progress-marker")) {
		t.Fatalf("curl failure lost progress/error stream: %q", output)
	}
}

func TestRepositoryInstallerReusesAndRepairsOnlyVerifiedBoundedDownloads(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	downloadCache := newDownloadCache(t)
	cacheEnvironment := "OBERTH_DOWNLOAD_CACHE=" + downloadCache

	if output, err := fixture.command(cacheEnvironment).CombinedOutput(); err != nil {
		t.Fatalf("populate verified download cache: %v\n%s", err, output)
	}
	cached := filepath.Join(downloadCache, ".oberth-download-cosign")
	if info, err := os.Lstat(cached); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("cached download = %#v, %v; want regular file", info, err)
	}
	resetCosignFixture(t, fixture)
	if output, err := fixture.command(cacheEnvironment, "OBERTH_CURL_EXIT=37").CombinedOutput(); err != nil {
		t.Fatalf("reuse verified download without curl: %v\n%s", err, output)
	} else if !bytes.Contains(output, []byte("install-tools: restored verified download cosign-linux-amd64-3.1.1\n")) {
		t.Fatalf("verified cache reuse is not observable: %q", output)
	}
	if _, err := os.Stat(fixture.argsPath); !os.IsNotExist(err) {
		t.Fatalf("verified cache reuse invoked curl: %v", err)
	}

	tampered := bytes.Repeat([]byte("x"), len(cosignFixturePayload))
	copy(tampered, "tampered")
	if err := os.WriteFile(cached, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	resetCosignFixture(t, fixture)
	if output, err := fixture.command(cacheEnvironment).CombinedOutput(); err != nil {
		t.Fatalf("repair corrupt cache from verified source: %v\n%s", err, output)
	}
	if _, err := os.Stat(fixture.argsPath); err != nil {
		t.Fatalf("corrupt cache did not fall back to curl: %v", err)
	}
	got, err := os.ReadFile(cached)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(cosignFixturePayload)) {
		t.Fatalf("repaired cache = %q, want verified fixture", got)
	}
	resetCosignFixture(t, fixture)
	if output, err := fixture.command(cacheEnvironment, "OBERTH_CURL_EXIT=37").CombinedOutput(); err != nil {
		t.Fatalf("reuse repaired cache without curl: %v\n%s", err, output)
	}
	if _, err := os.Stat(fixture.argsPath); !os.IsNotExist(err) {
		t.Fatalf("repaired cache reuse invoked curl: %v", err)
	}
}

func TestRepositoryInstallerKeepsCrashResidueBounded(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	downloadCache := newDownloadCache(t)
	stage := filepath.Join(downloadCache, ".oberth-download-cosign.stage")
	lock := filepath.Join(downloadCache, ".oberth-download-cosign.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(stage, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	cacheEnvironment := "OBERTH_DOWNLOAD_CACHE=" + downloadCache
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			resetCosignFixture(t, fixture)
		}
		output, err := fixture.command(cacheEnvironment).CombinedOutput()
		if err != nil {
			t.Fatalf("verified install with crash-left cache residue, attempt %d: %v\n%s", attempt, err, output)
		}
		if !bytes.Contains(output, []byte("download cache publication unavailable for cosign")) {
			t.Fatalf("crash-left cache lock is not reported on attempt %d: %q", attempt, output)
		}
	}
	entries, err := os.ReadDir(downloadCache)
	if err != nil {
		t.Fatal(err)
	}
	gotNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotNames = append(gotNames, entry.Name())
	}
	slices.Sort(gotNames)
	wantNames := []string{".oberth-download-cosign.lock", ".oberth-download-cosign.stage"}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("cache entries after repeated crash recovery = %v, want fixed bounded residue %v", gotNames, wantNames)
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil || !stageInfo.Mode().IsRegular() || stageInfo.Size() != 64<<20 {
		t.Fatalf("crash-left stage = %v, %v; want unchanged bounded regular file", stageInfo, err)
	}
	if _, err := os.Lstat(filepath.Join(downloadCache, ".oberth-download-cosign")); !os.IsNotExist(err) {
		t.Fatalf("contended crash cache unexpectedly published a final slot: %v", err)
	}
}

func TestRepositoryInstallerDoesNotReclaimYoungPublicationLock(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	downloadCache := newDownloadCache(t)
	lock := filepath.Join(downloadCache, ".oberth-download-cosign.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := fixture.command("OBERTH_DOWNLOAD_CACHE=" + downloadCache).CombinedOutput()
	if err != nil {
		t.Fatalf("active cache publisher blocked verified installation: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("download cache publication unavailable for cosign")) {
		t.Fatalf("cache-lock contention is not observable: %q", output)
	}
	info, err := os.Lstat(lock)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("young publication lock = %v, %v; want untouched physical directory", info, err)
	}
	for _, absent := range []string{
		filepath.Join(downloadCache, ".oberth-download-cosign.stage"),
		filepath.Join(downloadCache, ".oberth-download-cosign"),
	} {
		if _, err := os.Lstat(absent); !os.IsNotExist(err) {
			t.Fatalf("contending publisher created %s: %v", absent, err)
		}
	}
}

func TestRepositoryInstallerPublishesSharedCacheConcurrently(t *testing.T) {
	const installers = 8
	downloadCache := newDownloadCache(t)
	fixtures := make([]cosignFetchFixture, installers)
	for index := range fixtures {
		fixtures[index] = newCosignFetchFixture(t)
	}
	type result struct {
		output []byte
		err    error
	}
	results := make([]result, installers)
	start := make(chan struct{})
	releaseFetches := filepath.Join(t.TempDir(), "release-fetches")
	var ready sync.WaitGroup
	var complete sync.WaitGroup
	ready.Add(installers)
	complete.Add(installers)
	for index := range fixtures {
		go func() {
			defer complete.Done()
			ready.Done()
			<-start
			results[index].output, results[index].err = fixtures[index].command(
				"OBERTH_DOWNLOAD_CACHE="+downloadCache,
				"OBERTH_CURL_WAIT="+releaseFetches,
			).CombinedOutput()
		}()
	}
	ready.Wait()
	close(start)
	deadline := time.Now().Add(5 * time.Second)
	for {
		missing := 0
		for _, fixture := range fixtures {
			if _, err := os.Stat(fixture.argsPath); os.IsNotExist(err) {
				missing++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if missing == 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = os.WriteFile(releaseFetches, nil, 0o600)
			complete.Wait()
			t.Fatalf("only %d/%d concurrent installers reached curl after a cache miss", installers-missing, installers)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(releaseFetches, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	complete.Wait()
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("concurrent installer %d: %v\n%s", index, result.err, result.output)
		}
	}
	cached, err := os.ReadFile(filepath.Join(downloadCache, ".oberth-download-cosign"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, []byte(cosignFixturePayload)) {
		t.Fatalf("concurrently published cache = %q, want verified fixture", cached)
	}
	for _, fixture := range fixtures {
		installed, err := os.ReadFile(filepath.Join(fixture.toolsDir, "bin", "cosign"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(installed, cached) {
			t.Fatal("concurrent installer used bytes different from the canonical cache entry")
		}
	}
	for _, fixture := range fixtures {
		assertNoInstallerTemps(t, fixture, downloadCache)
	}
}

func TestRepositoryInstallerBoundsUntrustedCacheEntries(t *testing.T) {
	for _, test := range []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{
			name: "oversized sparse file",
			plant: func(t *testing.T, path string) {
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(64 << 20); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink entry",
			plant: func(t *testing.T, path string) {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte(cosignFixturePayload), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCosignFetchFixture(t)
			downloadCache := newDownloadCache(t)
			test.plant(t, filepath.Join(downloadCache, ".oberth-download-cosign"))
			output, err := fixture.command(
				"OBERTH_DOWNLOAD_CACHE="+downloadCache,
				"OBERTH_CURL_EXIT=37",
			).CombinedOutput()
			assertExitCode(t, err, 37, output)
			assertNoInstallerTemps(t, fixture, downloadCache)
		})
	}
}

func TestRepositoryInstallerRejectsCacheEntrySwapAfterOpen(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	downloadCache := newDownloadCache(t)
	cached := filepath.Join(downloadCache, ".oberth-download-cosign")
	if err := os.WriteFile(cached, []byte(cosignFixturePayload), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(target, []byte(cosignFixturePayload), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.toolsDir, "cache-was-swapped")
	output, err := fixture.command(
		"OBERTH_DOWNLOAD_CACHE="+downloadCache,
		"OBERTH_CACHE_SWAP_ON_STAT="+cached,
		"OBERTH_CACHE_SWAP_TARGET="+target,
		"OBERTH_CACHE_SWAP_MARKER="+marker,
		"OBERTH_CURL_EXIT=37",
	).CombinedOutput()
	assertExitCode(t, err, 37, output)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache swap hook did not execute: %v", err)
	}
	assertNoInstallerTemps(t, fixture, downloadCache)
}

func TestRepositoryInstallerBoundsFIFOReplacementBeforeOpen(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	downloadCache := newDownloadCache(t)
	cached := filepath.Join(downloadCache, ".oberth-download-cosign")
	if err := os.WriteFile(cached, []byte(cosignFixturePayload), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.toolsDir, "cache-became-fifo")
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Second)
	defer cancel()
	output, err := fixture.commandContext(ctx,
		"OBERTH_DOWNLOAD_CACHE="+downloadCache,
		"OBERTH_CACHE_FIFO_ON_TIMEOUT="+cached,
		"OBERTH_CACHE_FIFO_MARKER="+marker,
		"OBERTH_CURL_EXIT=37",
	).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("cache FIFO replacement exceeded the bounded open: %v; output=%q", ctx.Err(), output)
	}
	assertExitCode(t, err, 37, output)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache FIFO hook did not execute: %v", err)
	}
	assertNoInstallerTemps(t, fixture, downloadCache)
}

func TestRepositoryInstallerCleansFailedFetchAndPublication(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	downloadCache := newDownloadCache(t)
	cacheEnvironment := "OBERTH_DOWNLOAD_CACHE=" + downloadCache
	output, err := fixture.command(
		cacheEnvironment,
		"OBERTH_CURL_PARTIAL=1",
		"OBERTH_CURL_EXIT=37",
	).CombinedOutput()
	assertExitCode(t, err, 37, output)
	assertNoInstallerTemps(t, fixture, downloadCache)

	output, err = fixture.command(cacheEnvironment, "OBERTH_MV_FAIL_CACHE=1").CombinedOutput()
	if err != nil {
		t.Fatalf("best-effort cache publication failed installation: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("download cache publication unavailable for cosign")) {
		t.Fatalf("failed cache publication is not observable: %q", output)
	}
	assertNoInstallerTemps(t, fixture, downloadCache)
}

func TestRepositoryInstallerHandlesDirectoryValuedCacheSlots(t *testing.T) {
	for _, asSymlink := range []bool{false, true} {
		name := "directory"
		if asSymlink {
			name = "symlink to directory"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newCosignFetchFixture(t)
			downloadCache := newDownloadCache(t)
			slot := filepath.Join(downloadCache, ".oberth-download-cosign")
			if asSymlink {
				target := filepath.Join(downloadCache, "slot-target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, slot); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(slot, 0o700); err != nil {
				t.Fatal(err)
			}

			output, err := fixture.command("OBERTH_DOWNLOAD_CACHE=" + downloadCache).CombinedOutput()
			if err != nil {
				t.Fatalf("directory-valued cache slot broke installation: %v\n%s", err, output)
			}
			info, err := os.Lstat(slot)
			if err != nil {
				t.Fatal(err)
			}
			if asSymlink {
				if info.Mode()&os.ModeSymlink == 0 ||
					!bytes.Contains(output, []byte("download cache publication unavailable for cosign")) {
					t.Fatalf("symlink-valued slot did not remain intact and degrade cleanly: mode=%v output=%q", info.Mode(), output)
				}
			} else if !info.IsDir() || !bytes.Contains(output, []byte("download cache publication unavailable for cosign")) {
				t.Fatalf("directory-valued slot did not degrade cleanly: mode=%v output=%q", info.Mode(), output)
			}
			assertNoInstallerTemps(t, fixture, downloadCache)
		})
	}
}

func TestRepositoryInstallerRejectsUnsafeDownloadCacheRoots(t *testing.T) {
	fixture := newCosignFetchFixture(t)
	canonical := newDownloadCache(t)
	noncanonical := canonical + "/../" + filepath.Base(canonical)
	directSymlink := filepath.Join("/tmp", "oberth-download-cache-link-"+filepath.Base(fixture.toolsDir))
	if err := os.Symlink(t.TempDir(), directSymlink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(directSymlink) })
	ancestor, err := os.MkdirTemp("/tmp", "oberth-download-cache-ancestor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ancestor) })
	ancestorTarget := t.TempDir()
	if err := os.Mkdir(filepath.Join(ancestorTarget, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ancestorTarget, filepath.Join(ancestor, "link")); err != nil {
		t.Fatal(err)
	}
	for name, cachePath := range map[string]string{
		"unscoped":           "/tmp/download-cache-outside-contract",
		"wrong mount":        "/cache/gomod",
		"mount suffix":       "/cache/gobuild-extra",
		"noncanonical":       noncanonical,
		"direct symlink":     directSymlink,
		"symlinked ancestor": filepath.Join(ancestor, "link", "cache"),
	} {
		t.Run(name, func(t *testing.T) {
			output, err := fixture.command("OBERTH_DOWNLOAD_CACHE=" + cachePath).CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte("OBERTH_DOWNLOAD_CACHE")) {
				t.Fatalf("unsafe cache error = %v, output=%q", err, output)
			}
		})
	}
}

func newDownloadCache(t *testing.T) string {
	t.Helper()
	downloadCache, err := os.MkdirTemp("/tmp", "oberth-download-cache-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(downloadCache) })
	return downloadCache
}

func resetCosignFixture(t *testing.T, fixture cosignFetchFixture) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(fixture.toolsDir, "downloads"),
		filepath.Join(fixture.toolsDir, "bin", "cosign"),
		fixture.argsPath,
	} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

func assertExitCode(t *testing.T, err error, want int, output []byte) {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != want {
		t.Fatalf("installer error = %v, want exit %d; output=%q", err, want, output)
	}
}

func assertNoInstallerTemps(t *testing.T, fixture cosignFetchFixture, downloadCache string) {
	t.Helper()
	for _, pattern := range []string{
		filepath.Join(fixture.toolsDir, "downloads", "*.partial.*"),
		filepath.Join(fixture.toolsDir, "downloads", "*.status"),
		filepath.Join(downloadCache, ".oberth-download-*.tmp.*"),
		filepath.Join(downloadCache, ".oberth-download-*.stage"),
		filepath.Join(downloadCache, ".oberth-download-*.lock"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("installer left temporary files matching %q: %v", pattern, matches)
		}
	}
	if err := filepath.WalkDir(downloadCache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".oberth-download-") &&
			(strings.Contains(entry.Name(), ".tmp.") || strings.HasSuffix(entry.Name(), ".stage") ||
				strings.HasSuffix(entry.Name(), ".lock")) {
			return fmt.Errorf("installer left nested cache temporary %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type cosignFetchFixture struct {
	toolsDir    string
	stubDir     string
	argsPath    string
	releasePath string
}

func newCosignFetchFixture(t *testing.T) cosignFetchFixture {
	t.Helper()
	toolsDir, err := os.MkdirTemp("/tmp", "oberth-tools-fetch-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolsDir) })
	fixture := cosignFetchFixture{
		toolsDir: toolsDir, stubDir: t.TempDir(),
		argsPath: filepath.Join(toolsDir, "curl-args"), releasePath: filepath.Join(toolsDir, "release-fetch"),
	}
	writeTestExecutable(t, filepath.Join(fixture.stubDir, "curl"), `#!/bin/sh
printf '%s\n' "$@" >"$OBERTH_CURL_ARGS"
output=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --silent) exit 92 ;;
    --output)
      shift
      output=$1
      ;;
  esac
  shift
done
[ -n "$output" ] || exit 93
# curl prefixes each in-place progress frame with a carriage return. A stalled
# first frame must still give the predecessor a newline heartbeat immediately.
printf '\rfetch-progress-marker' >&2
if [ -n "${OBERTH_CURL_WAIT:-}" ]; then
  while [ ! -f "$OBERTH_CURL_WAIT" ]; do sleep 0.01; done
fi
if [ "${OBERTH_CURL_PARTIAL:-0}" -eq 1 ]; then
  printf 'partial' >"$output"
fi
if [ "${OBERTH_CURL_EXIT:-0}" -ne 0 ]; then
  exit "$OBERTH_CURL_EXIT"
fi
cat >"$output" <<'EOF'
#!/bin/sh
printf 'GitVersion:    v3.1.1\n'
EOF
chmod 0700 "$output"
`)
	writeTestExecutable(t, filepath.Join(fixture.stubDir, "sha256sum"), `#!/bin/sh
if grep -q '^tampered' "$1"; then
  printf '0000000000000000000000000000000000000000000000000000000000000000  %s\n' "$1"
  exit 0
fi
printf 'ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc  %s\n' "$1"
`)
	busyboxPath, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, filepath.Join(fixture.stubDir, "mv"), fmt.Sprintf(`#!/bin/sh
destination=
for argument do destination=$argument; done
if [ "${OBERTH_MV_FAIL_CACHE:-0}" -eq 1 ]; then
  case "$destination" in
    */.oberth-download-cosign) exit 73 ;;
  esac
fi
exec %q mv "$@"
`, busyboxPath))
	lnPath, err := exec.LookPath("ln")
	if err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, filepath.Join(fixture.stubDir, "busybox"), fmt.Sprintf(`#!/bin/sh
last=
for argument do last=$argument; done
if [ "${1:-}" = timeout ] && [ -n "${OBERTH_CACHE_FIFO_ON_TIMEOUT:-}" ] &&
  [ ! -e "$OBERTH_CACHE_FIFO_MARKER" ]; then
  : >"$OBERTH_CACHE_FIFO_MARKER"
  %q mv "$OBERTH_CACHE_FIFO_ON_TIMEOUT" "$OBERTH_CACHE_FIFO_ON_TIMEOUT.before-fifo"
  %q mkfifo "$OBERTH_CACHE_FIFO_ON_TIMEOUT"
fi
if [ "${1:-}" = stat ] && [ -n "${OBERTH_CACHE_SWAP_ON_STAT:-}" ] &&
  [ "$last" = "$OBERTH_CACHE_SWAP_ON_STAT" ] && [ ! -e "$OBERTH_CACHE_SWAP_MARKER" ]; then
  : >"$OBERTH_CACHE_SWAP_MARKER"
  %q mv "$last" "$last.before-swap"
  %q -s "$OBERTH_CACHE_SWAP_TARGET" "$last"
fi
exec %q "$@"
`, busyboxPath, busyboxPath, busyboxPath, lnPath, busyboxPath))
	return fixture
}

func (fixture cosignFetchFixture) command(extraEnv ...string) *exec.Cmd {
	return fixture.commandContext(context.Background(), extraEnv...)
}

func (fixture cosignFetchFixture) commandContext(ctx context.Context, extraEnv ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "busybox", "sh", "../../.oberth/install-tools.sh", "cosign")
	command.Env = append(os.Environ(),
		"OBERTH_TOOLS_DIR="+fixture.toolsDir,
		"OBERTH_CURL_ARGS="+fixture.argsPath,
		fmt.Sprintf("OBERTH_TEST_DOWNLOAD_SIZE=%d", len(cosignFixturePayload)),
		"PATH="+fixture.stubDir+":"+os.Getenv("PATH"),
	)
	command.Env = append(command.Env, extraEnv...)
	return command
}

func TestZigExtractionCannotBypassVerifiedXZ(t *testing.T) {
	const archiveFixture = "not an xz stream"
	toolsDir, err := os.MkdirTemp("/tmp", "oberth-tools-xz-bypass-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolsDir) })
	binDir := filepath.Join(toolsDir, "bin")
	downloadsDir := filepath.Join(toolsDir, "downloads")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(downloadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(toolsDir, "xz-invoked")
	writeTestExecutable(t, filepath.Join(binDir, "xz"), `#!/bin/sh
if [ "${1:-}" = "--version" ]; then
  echo "xz: version v0.5.16"
  exit 0
fi
: >"$OBERTH_XZ_MARKER"
exit 73
`)
	writeTestExecutable(t, filepath.Join(binDir, "sha256sum"), `#!/bin/sh
case "$1" in
  */bin/xz) digest=ac4688de237077cbade4c16cfc2ab32f22ba2ff4ffb8bc1ad39ade32dd09e696 ;;
  */downloads/zig-x86_64-linux-0.14.1.tar.xz) digest=24aeeec8af16c381934a6cd7d95c807a8cb2cf7df9fa40d359aa884195c4716c ;;
  *) exit 91 ;;
esac
printf '%s  %s\n' "$digest" "$1"
`)
	// This tar simulates an implementation whose -J decoder is in-process. An
	// implementation that delegates extraction to tar would therefore succeed
	// without ever executing the checksum-bound xz program.
	writeTestExecutable(t, filepath.Join(binDir, "tar"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-C" ]; then
    shift
    destination=$1
  fi
  shift
done
mkdir -p "$destination/zig-x86_64-linux-0.14.1"
cat >"$destination/zig-x86_64-linux-0.14.1/zig" <<'EOF'
#!/bin/sh
echo 0.14.1
EOF
chmod 0755 "$destination/zig-x86_64-linux-0.14.1/zig"
`)
	archive := filepath.Join(downloadsDir, "zig-x86_64-linux-0.14.1.tar.xz")
	if err := os.WriteFile(archive, []byte(archiveFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("busybox", "sh", "../../.oberth/install-tools.sh", "zig")
	command.Env = append(os.Environ(),
		"OBERTH_TOOLS_DIR="+toolsDir,
		fmt.Sprintf("OBERTH_TEST_DOWNLOAD_SIZE=%d", len(archiveFixture)),
		"OBERTH_XZ_MARKER="+marker,
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("Zig setup bypassed the failing verified xz executable:\n%s", output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("verified xz executable was not invoked: %v", err)
	}
}

func TestPythonInstallerIsPinnedAtomicAndIdempotent(t *testing.T) {
	t.Run("success, reuse, and substituted shim", func(t *testing.T) {
		fixture := newPythonInstallerFixture(t)
		if output, err := fixture.run(); err != nil {
			t.Fatalf("install Python: %v\n%s", err, output)
		}
		if _, err := os.Stat(fixture.executedMarker); err != nil {
			t.Fatalf("verified Python executable was not invoked: %v", err)
		}
		link, err := os.Readlink(filepath.Join(fixture.toolsDir, "bin", "python3"))
		if err != nil {
			t.Fatal(err)
		}
		if link != "../python/bin/python3" {
			t.Fatalf("published python3 link = %q", link)
		}
		if err := os.Remove(fixture.executedMarker); err != nil {
			t.Fatal(err)
		}
		if output, err := fixture.run(); err != nil {
			t.Fatalf("reuse Python: %v\n%s", err, output)
		}
		attempts, err := os.ReadFile(fixture.tarCounter)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(attempts)) != "1" {
			t.Fatalf("Python archive extraction attempts = %q, want 1", attempts)
		}

		innerLink := filepath.Join(fixture.toolsDir, "python", "bin", "python3")
		if err := os.Remove(innerLink); err != nil {
			t.Fatal(err)
		}
		maliciousMarker := filepath.Join(fixture.toolsDir, "substituted-python-ran")
		writeTestExecutable(t, innerLink, "#!/bin/sh\n: >\"$OBERTH_SUBSTITUTED_PYTHON\"\nexit 0\n")
		output, err := fixture.run("OBERTH_SUBSTITUTED_PYTHON=" + maliciousMarker)
		if err == nil {
			t.Fatalf("Python setup accepted a substituted python3 entry point:\n%s", output)
		}
		if !strings.Contains(string(output), "unexpected python3 entry point") {
			t.Fatalf("substituted python3 failure omitted its cause:\n%s", output)
		}
		if _, err := os.Stat(maliciousMarker); !os.IsNotExist(err) {
			t.Fatalf("substituted python3 was executed: %v", err)
		}
	})

	for _, test := range []struct {
		name        string
		environment string
		want        string
	}{
		{name: "archive checksum", environment: "OBERTH_PYTHON_BAD_ARCHIVE=1", want: "checksum mismatch"},
		{name: "binary checksum", environment: "OBERTH_PYTHON_BAD_BINARY=1", want: "checksum mismatch"},
		{name: "interpreter probe", environment: "OBERTH_PYTHON_COMMAND_FAIL=1", want: "installed Python version"},
		{name: "unexpected archive entry", environment: "OBERTH_PYTHON_EXTRA_ENTRY=1", want: "unexpected top-level entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPythonInstallerFixture(t)
			output, err := fixture.run(test.environment)
			if err == nil {
				t.Fatalf("Python setup accepted %s failure:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Fatalf("%s failure omitted %q:\n%s", test.name, test.want, output)
			}
			fixture.assertNoPublishedPython(t)
		})
	}

	t.Run("interrupted extraction cleans up and retries", func(t *testing.T) {
		fixture := newPythonInstallerFixture(t)
		output, err := fixture.run("OBERTH_PYTHON_TAR_FAIL=1")
		if err == nil {
			t.Fatalf("Python setup accepted interrupted extraction:\n%s", output)
		}
		fixture.assertNoPublishedPython(t)
		if output, err := fixture.run(); err != nil {
			t.Fatalf("Python setup did not recover after interrupted extraction: %v\n%s", err, output)
		}
	})
}

type pythonInstallerFixture struct {
	toolsDir       string
	executedMarker string
	tarCounter     string
	environment    []string
}

func newPythonInstallerFixture(t *testing.T) pythonInstallerFixture {
	t.Helper()
	const archiveFixture = "pinned Python archive fixture"
	toolsDir, err := os.MkdirTemp("/tmp", "oberth-tools-python-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolsDir) })
	binDir := filepath.Join(toolsDir, "bin")
	downloadsDir := filepath.Join(toolsDir, "downloads")
	for _, directory := range []string{binDir, downloadsDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(downloadsDir, "cpython-3.13.14+20260718-x86_64-unknown-linux-musl-install_only_stripped.tar.gz")
	if err := os.WriteFile(archive, []byte(archiveFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestExecutable(t, filepath.Join(binDir, "sha256sum"), `#!/bin/sh
case "$1" in
  */downloads/cpython-3.13.14+20260718-x86_64-unknown-linux-musl-install_only_stripped.tar.gz)
    digest=0dbfe4b9c043f81a60953245a2735594f6b09308031086ceac15511f4c0af193
    [ "${OBERTH_PYTHON_BAD_ARCHIVE:-}" != 1 ] || digest=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
    ;;
  */python/bin/python3.13)
    digest=c524653b6cc492851f59bf5572c81f3f9b518fa84b34e734c59ba06848d65a8f
    [ "${OBERTH_PYTHON_BAD_BINARY:-}" != 1 ] || digest=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
    ;;
  *) exit 91 ;;
esac
printf '%s  %s\n' "$digest" "$1"
`)
	writeTestExecutable(t, filepath.Join(binDir, "tar"), `#!/bin/sh
destination=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-C" ]; then
    shift
    destination=$1
  fi
  shift
done
[ -n "$destination" ] || exit 92
attempt=0
[ ! -f "$OBERTH_PYTHON_TAR_COUNTER" ] || attempt=$(cat "$OBERTH_PYTHON_TAR_COUNTER")
attempt=$((attempt + 1))
printf '%s\n' "$attempt" >"$OBERTH_PYTHON_TAR_COUNTER"
mkdir -p "$destination/python/bin"
[ "${OBERTH_PYTHON_TAR_FAIL:-}" != 1 ] || exit 73
cat >"$destination/python/bin/python3.13" <<'EOF'
#!/bin/sh
[ "$#" -eq 5 ] && [ "$1" = -I ] && [ "$2" = -S ] && [ "$3" = -B ] && [ "$4" = -c ] || exit 94
[ "${OBERTH_PYTHON_COMMAND_FAIL:-}" != 1 ] || exit 95
: >"$OBERTH_PYTHON_EXECUTED"
EOF
chmod 0755 "$destination/python/bin/python3.13"
ln -s python3.13 "$destination/python/bin/python3"
[ "${OBERTH_PYTHON_EXTRA_ENTRY:-}" != 1 ] || : >"$destination/unexpected"
`)
	executedMarker := filepath.Join(toolsDir, "verified-python-ran")
	tarCounter := filepath.Join(toolsDir, "tar-attempts")
	return pythonInstallerFixture{
		toolsDir:       toolsDir,
		executedMarker: executedMarker,
		tarCounter:     tarCounter,
		environment: append(os.Environ(),
			"OBERTH_TOOLS_DIR="+toolsDir,
			fmt.Sprintf("OBERTH_TEST_DOWNLOAD_SIZE=%d", len(archiveFixture)),
			"OBERTH_PYTHON_EXECUTED="+executedMarker,
			"OBERTH_PYTHON_TAR_COUNTER="+tarCounter,
			"PATH="+binDir+":"+os.Getenv("PATH"),
		),
	}
}

func (fixture pythonInstallerFixture) run(extraEnvironment ...string) ([]byte, error) {
	command := exec.Command("busybox", "sh", "../../.oberth/install-tools.sh", "python")
	command.Env = append(fixture.environment, extraEnvironment...)
	return command.CombinedOutput()
}

func (fixture pythonInstallerFixture) assertNoPublishedPython(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(fixture.toolsDir, "python")); !os.IsNotExist(err) {
		t.Fatalf("failed Python installation was published: %v", err)
	}
	staging, err := filepath.Glob(filepath.Join(fixture.toolsDir, ".python.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("failed Python installation left staging paths: %v", staging)
	}
}

func TestModuleDownloadRetriesTransientFailure(t *testing.T) {
	toolsDir, err := os.MkdirTemp("/tmp", "oberth-tools-module-retry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolsDir) })
	binDir := filepath.Join(toolsDir, "bin")
	goBinDir := filepath.Join(toolsDir, "go", "bin")
	moduleCache := filepath.Join(toolsDir, "gomod")
	for _, directory := range []string{binDir, goBinDir, moduleCache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	counter := filepath.Join(toolsDir, "download-attempts")
	writeTestExecutable(t, filepath.Join(goBinDir, "go"), `#!/bin/sh
if [ "$1" = mod ] && [ "$2" = download ]; then
  if [ "$GOPATH" != "$OBERTH_TOOLS_DIR/gopath" ] || [ ! -d "$GOPATH" ] || [ ! -w "$GOPATH" ]; then
    printf '%s\n' '{"Error":"GOPATH is not the writable repository-owned path"}'
    exit 93
  fi
  attempt=0
  [ ! -f "$OBERTH_GO_COUNTER" ] || attempt=$(cat "$OBERTH_GO_COUNTER")
  attempt=$((attempt + 1))
  printf '%s\n' "$attempt" >"$OBERTH_GO_COUNTER"
  if [ "$attempt" -lt 3 ] || [ "${OBERTH_GO_ALWAYS_FAIL:-}" = 1 ]; then
    printf '%s\n' '{"Error":"transient module proxy failure"}'
    exit 1
  fi
  mkdir -p "$GOMODCACHE/github.com/ulikunitz/xz@v0.5.16/cmd/gxz"
  printf '%s\n' '{"Sum": "h1:ld6NyySjx5lowVKwJvMRLnW5nxKX/xnpSiFYZ/Lxur0=", "GoModSum": "h1:H9Rt/W6/Qj27PGauhQc6nfCDy7vHpzsOThBSaYDoEhw="}'
  exit 0
fi
if [ "$1" = env ] && [ "$2" = GOMODCACHE ]; then
  printf '%s\n' "$GOMODCACHE"
  exit 0
fi
if [ "$1" = build ]; then
  while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then
      shift
      output=$1
      break
    fi
    shift
  done
  cat >"$output" <<'EOF'
#!/bin/sh
echo 'xz: version v0.5.16'
EOF
  chmod 0755 "$output"
  exit 0
fi
exit 92
`)
	writeTestExecutable(t, filepath.Join(binDir, "sha256sum"), `#!/bin/sh
printf '%s  %s\n' ac4688de237077cbade4c16cfc2ab32f22ba2ff4ffb8bc1ad39ade32dd09e696 "$1"
`)
	writeTestExecutable(t, filepath.Join(binDir, "sleep"), "#!/bin/sh\nexit 0\n")
	environment := append(os.Environ(),
		"OBERTH_TOOLS_DIR="+toolsDir,
		"OBERTH_GO_COUNTER="+counter,
		"GOMODCACHE="+moduleCache,
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)
	command := exec.Command("../../.oberth/install-tools.sh", "xz")
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("xz setup did not recover from transient module download failures: %v\n%s", err, output)
	}
	attempts, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(attempts)) != "3" {
		t.Fatalf("module download attempts = %q, want 3", attempts)
	}
	if err := os.Remove(filepath.Join(binDir, "xz")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(counter, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := exec.Command("../../.oberth/install-tools.sh", "xz")
	failure.Env = append(environment, "OBERTH_GO_ALWAYS_FAIL=1")
	output, err := failure.CombinedOutput()
	if err == nil {
		t.Fatalf("xz setup accepted a permanently failed module download:\n%s", output)
	}
	for _, expected := range []string{`{"Error":"transient module proxy failure"}`, "download failed after 3 attempts"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("permanent module download failure omitted %q:\n%s", expected, output)
		}
	}
}

func writeTestExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
