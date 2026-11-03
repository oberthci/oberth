package argojob

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCloudflarePublicationPhasesAreClosedAndOrdered(t *testing.T) {
	workflow := builtOberthRelease(t)
	leaf := releaseTemplate(t, workflow, "release-website")
	if len(leaf.Inputs.Parameters) != 1 || leaf.Inputs.Parameters[0].Name != "phase" {
		t.Fatal("Cloudflare leaf must accept only one fixed phase parameter")
	}
	var phases []string
	for _, value := range leaf.Inputs.Parameters[0].Enum {
		phases = append(phases, value.String())
	}
	if !reflect.DeepEqual(phases, []string{"publish-public", "finalize", "publish-website"}) {
		t.Fatalf("unexpected Cloudflare phases: %v", phases)
	}
	want := map[string]struct{ phase, dependency string }{
		"release-publish-public": {"publish-public", "release-publish-chart"},
		"release-finalize":       {"finalize", "release-runtime"},
		"release-website":        {"publish-website", "release-finalize"},
	}
	for _, task := range releaseTemplate(t, workflow, "release").DAG.Tasks {
		if task.Template != "release-website" {
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
