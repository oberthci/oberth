package argojob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseHelperDigestUsesSignedMetadataAndKeepsChecksums(t *testing.T) {
	f := newReleaseRuntimeFixture(t)
	f.tool("helm", immutableChartFixture)
	before, err := os.ReadFile(filepath.Join(f.candidate, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := f.run("publish-chart", true); err != nil {
		t.Fatalf("publish signed release metadata: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(f.candidate, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Images map[string]struct{ Ref string }
		Notes  string
	}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(filepath.Join(f.candidate, "helper-image.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimSpace(string(helper))
	if record.Images["helper"].Ref != ref || !strings.Contains(record.Notes, ref) {
		t.Fatal("signed release record omitted the helper reference or release note")
	}
	if bundle, err := os.ReadFile(filepath.Join(f.candidate, "release.json.sigstore.json")); err != nil || len(bundle) == 0 {
		t.Fatal("release metadata lacks its signature", err)
	}
	after, err := os.ReadFile(filepath.Join(f.candidate, "SHA256SUMS"))
	if err != nil || string(after) != string(before) || strings.Count(string(after), "\n") != 5 {
		t.Fatal("helper changed the five-file checksum contract", err)
	}
}

func TestReleaseHelperScanFailurePreventsEitherImageSignature(t *testing.T) {
	f := newReleaseRuntimeFixture(t)
	f.tool("trivy", `for reference do :; done
printf 'scan:%s\n' "$reference" >>"$FIXTURE_TRACE"
case "$reference" in *oberth-helper@*) exit 25;; esac`)
	out, err := f.run("publish-images", true)
	if err == nil || !strings.Contains(f.log(), "oberth-helper@") || strings.Contains(f.log(), "cosign:sign:") {
		t.Fatalf("helper scan failure reached signing: %v\n%s\n%s", err, out, f.log())
	}
}

func TestReleaseHelperRejectsForeignReferenceBeforeMetadataSigning(t *testing.T) {
	for _, value := range []string{
		"example.invalid/oberth-helper@sha256:" + strings.Repeat("b", 64),
		"europe-west4-docker.pkg.dev/skipopsmain/oberth/other-helper@sha256:" + strings.Repeat("b", 64),
		"europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-helper:v1.2.3",
		"europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-helper@sha256:short",
		"europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-helper@sha256:" + strings.Repeat("b", 64) + "@sha256:" + strings.Repeat("b", 64),
	} {
		t.Run(value, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			f.tool("helm", immutableChartFixture)
			f.absolute(filepath.Join(f.candidate, "helper-image.txt"), value+"\n", 0600)
			for _, file := range []string{"release.json", "release.json.sigstore.json"} {
				if err := os.Remove(filepath.Join(f.candidate, file)); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.run("publish-chart", true)
			if err == nil || !strings.Contains(out, "helper image reference is not an immutable Oberth GAR digest") {
				t.Fatalf("invalid helper reference reached metadata: %v\n%s", err, out)
			}
			for _, file := range []string{"release.json", "release.json.sigstore.json"} {
				if _, err := os.Stat(filepath.Join(f.candidate, file)); !os.IsNotExist(err) {
					t.Fatal("invalid helper reference produced release metadata", file, err)
				}
			}
		})
	}
}
