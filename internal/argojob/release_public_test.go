package argojob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCloudflarePublicationPhasesAreClosedAndOrdered(t *testing.T) {
	workflow := builtOberthRelease(t)
	leaf := releaseTemplate(t, workflow, "release-public-artifacts")
	if len(leaf.Inputs.Parameters) != 1 || leaf.Inputs.Parameters[0].Name != "phase" {
		t.Fatal("Cloudflare leaf must accept only one fixed phase parameter")
	}
	var phases []string
	for _, value := range leaf.Inputs.Parameters[0].Enum {
		phases = append(phases, value.String())
	}
	if !reflect.DeepEqual(phases, []string{"publish-public", "finalize"}) {
		t.Fatalf("unexpected Cloudflare phases: %v", phases)
	}
	want := map[string]struct{ phase, dependency string }{
		"release-publish-public": {"publish-public", "release-publish-chart"},
		"release-finalize":       {"finalize", "release-runtime"},
	}
	for _, task := range releaseTemplate(t, workflow, "release").DAG.Tasks {
		if task.Template != "release-public-artifacts" {
			continue
		}
		expected, found := want[task.Name]
		if !found || task.Depends != expected.dependency || len(task.Arguments.Parameters) != 1 || task.Arguments.Parameters[0].Name != "phase" || task.Arguments.Parameters[0].Value == nil || task.Arguments.Parameters[0].Value.String() != expected.phase {
			t.Fatalf("unexpected Cloudflare task: %+v", task)
		}
		delete(want, task.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing Cloudflare tasks: %v", want)
	}
	fixture := newReleaseRuntimeFixture(t)
	if output, err := fixture.run("cloudflare-phase", true, "publish-images"); err == nil || !strings.Contains(output, "unsupported Cloudflare publication phase") || fixture.log() != "" {
		t.Fatalf("arbitrary credentialed phase accepted: %v %s %s", err, output, fixture.log())
	}
}

func TestPublicPublicationAuthenticatesEveryPayloadBeforeWriting(t *testing.T) {
	for _, payload := range []string{"SHA256SUMS", "release.json", "oberth-1.2.3.tgz"} {
		t.Run(payload, func(t *testing.T) {
			fixture := newReleaseRuntimeFixture(t)
			fixture.absolute(filepath.Join(fixture.candidate, "oberth-1.2.3.tgz.sigstore.json"), "valid", 0600)
			fixture.absolute(filepath.Join(fixture.candidate, payload+".sigstore.json"), "invalid", 0600)
			output, err := fixture.run("publish-public", true)
			if err == nil || !strings.Contains(output, "valid staged signature") || strings.Contains(fixture.log(), "curl:") {
				t.Fatalf("invalid %s reached publication: %v %s %s", payload, err, output, fixture.log())
			}
		})
	}
}

func TestPublicSnapshotRefusesUntrustedFileTypesAndMissingProof(t *testing.T) {
	for _, scenario := range []string{"symlink", "hardlink", "missing", "missing-receipt"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newReleaseRuntimeFixture(t)
			fixture.absolute(filepath.Join(fixture.candidate, "oberth-1.2.3.tgz.sigstore.json"), "valid", 0600)
			// Only relocate the fixed input mount to this test's private directory;
			// execute the complete source-owned phase/snapshot implementation.
			script, err := os.ReadFile(fixture.script)
			if err != nil {
				t.Fatal(err)
			}
			fixture.script = filepath.Join(fixture.root, "release.sh")
			fixture.absolute(fixture.script, strings.ReplaceAll(string(script), "/tmp/oberth-release-inputs", fixture.candidate), 0700)
			file := filepath.Join(fixture.candidate, "oberth-linux-arm64")
			if scenario != "missing-receipt" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "symlink":
				err = os.Symlink(filepath.Join(fixture.candidate, "oberth-linux-amd64"), file)
			case "hardlink":
				err = os.Link(filepath.Join(fixture.candidate, "oberth-linux-amd64"), file)
			}
			if err != nil {
				t.Fatal(err)
			}
			phase, diagnostic := "publish-public", "cannot snapshot signed public inputs"
			if scenario == "missing-receipt" {
				phase, diagnostic = "finalize", "matching tokenless runtime receipt"
			}
			output, err := fixture.run("cloudflare-phase", true, phase)
			if err == nil || !strings.Contains(output, diagnostic) || strings.Contains(fixture.log(), "curl:") {
				t.Fatalf("unsafe %s reached cloud publication: %v %s %s", scenario, err, output, fixture.log())
			}
		})
	}
}

// Both signature forms, the authenticated bundle digest and the release
// record must agree before the first remote write. Crypto uses an ephemeral
// test key; only external Sigstore services are faked by the fixture.
func TestPublicChecksumProofsRefuseDisagreement(t *testing.T) {
	for _, scenario := range []string{"signature", "bundle-digest", "bundle-signature", "record-binary", "record-chart", "record-version"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newReleaseRuntimeFixture(t)
			fixture.absolute(filepath.Join(fixture.candidate, "oberth-1.2.3.tgz.sigstore.json"), "valid", 0600)
			name := "SHA256SUMS.sig"
			switch scenario {
			case "signature":
				fixture.absolute(filepath.Join(fixture.candidate, name), "invalid", 0600)
			case "bundle-digest", "bundle-signature":
				name = "SHA256SUMS.sigstore.json"
				var bundle map[string]any
				if err := json.Unmarshal(mustReleaseFile(t, filepath.Join(fixture.candidate, name)), &bundle); err != nil {
					t.Fatal(err)
				}
				message := bundle["messageSignature"].(map[string]any)
				if scenario == "bundle-digest" {
					message["messageDigest"].(map[string]any)["digest"] = strings.Repeat("A", 44)
				} else {
					message["signature"] = strings.Repeat("A", 96)
				}
				body, err := json.Marshal(bundle)
				if err != nil {
					t.Fatal(err)
				}
				fixture.absolute(filepath.Join(fixture.candidate, name), string(body), 0600)
			default:
				name = "release.json"
				var record map[string]any
				if err := json.Unmarshal(mustReleaseFile(t, filepath.Join(fixture.candidate, name)), &record); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "record-binary":
					record["artifacts"].(map[string]any)["oberth-linux-amd64"] = "sha256:" + strings.Repeat("0", 64)
				case "record-chart":
					record["chart"].(map[string]any)["digest"] = "sha256:" + strings.Repeat("0", 64)
				case "record-version":
					record["version"] = "v9.9.9"
				}
				body, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				fixture.absolute(filepath.Join(fixture.candidate, name), string(body), 0600)
			}
			output, err := fixture.run("publish-public", true)
			if err == nil || strings.Contains(fixture.log(), "curl:") {
				t.Fatalf("%s reached publication: %v %s %s", scenario, err, output, fixture.log())
			}
		})
	}
}

func mustReleaseFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPublicChecksumSignatureRetryReusesAuthoritativeBundle(t *testing.T) {
	fixture := newReleaseRuntimeFixture(t)
	fixture.absolute(filepath.Join(fixture.candidate, "oberth-1.2.3.tgz.sigstore.json"), "valid", 0600)
	previous := mustReleaseFile(t, filepath.Join(fixture.root, "objects/oberth/v1.2.3/SHA256SUMS.sig"))
	if output, err := fixture.run("sign-binaries", true); err != nil {
		t.Fatalf("sign: %v %s", err, output)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if output, err := fixture.run("publish-public", true); err != nil {
			t.Fatalf("publish: %v %s %s", err, output, fixture.log())
		}
		current := mustReleaseFile(t, filepath.Join(fixture.root, "objects/oberth/v1.2.3/SHA256SUMS.sig"))
		if string(current) != string(previous) {
			t.Fatal("retry replaced the immutable DER signature")
		}
	}
}
