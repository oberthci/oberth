package argojob

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReleaseWIFConsumerRoleAndCleanEnvironment(t *testing.T) {
	wf, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"release-publish-images": "image-writer", "release-publish-kvm-harness": "image-writer", "release-publish-chart": "chart-writer", "release-verify": "image-reader"}
	for _, leaf := range wf.Spec.Templates {
		role, ok := want[leaf.Name]
		if !ok {
			continue
		}
		delete(want, leaf.Name)
		if leaf.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation] != role {
			t.Errorf("%s role mismatch", leaf.Name)
		}
		if leaf.Container == nil {
			t.Fatalf("%s must be container", leaf.Name)
		}
		args := strings.Join(leaf.Container.Args, "\n")
		for _, env := range []string{`OBERTH_RELEASE_WIF="${OBERTH_RELEASE_WIF:-}"`, `GOOGLE_APPLICATION_CREDENTIALS=/run/oberth-gar/adc.json`} {
			if !strings.Contains(args, env) {
				t.Errorf("%s loses %s", leaf.Name, env)
			}
		}
		if strings.Contains(args, "--path=oberth/data/release/gar-") {
			t.Errorf("%s fetches a GAR key", leaf.Name)
		}
		if leaf.Name == "release-verify" && templateUsesOberthSecretstore(&leaf) {
			t.Fatal("keyless verifier invokes secretstore without a secret")
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing leaves %v", want)
	}
}

func TestAcceptedReleaseWIFExchangeFixtures(t *testing.T) {
	cmd := exec.Command("/usr/bin/python3", "-I", "-B", filepath.Join(repositoryRoot(t), "hack/test-release-wif.py"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("accepted helper exchange fixtures: %v\n%s", err, out)
	}
}

func TestReleaseWIFInvalidMarkerNeverUsesLegacyKeys(t *testing.T) {
	for _, action := range []string{"publish-images", "publish-chart", "verify", "publish-kvm-harness"} {
		t.Run(action, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			f.env = []string{"OBERTH_RELEASE_WIF=invalid"}
			out, err := f.run(action, true)
			if err == nil || !strings.Contains(out, "invalid OBERTH_RELEASE_WIF") {
				t.Fatalf("invalid marker was not rejected: %v\n%s", err, out)
			}
			if f.log() != "" {
				t.Fatalf("invalid marker reached external tools: %s", f.log())
			}
		})
	}
}

func TestReleaseWIFConfigHasNoCredentialFiles(t *testing.T) {
	for _, action := range []string{"publish-images", "publish-chart", "verify", "publish-kvm-harness"} {
		t.Run(action, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			source, err := os.ReadFile(f.script)
			if err != nil {
				t.Fatal(err)
			}
			// Execute the production auth functions directly, avoiding registry/build
			// effects. The main dispatch is the only replaced part of the script.
			index := strings.LastIndex(string(source), "\ncase \"$action\" in\n")
			if index < 0 {
				t.Fatal("missing action dispatch")
			}
			script := string(source[:index]) + `
if [ "$action" = publish-kvm-harness ]; then prepare_kvm_registry_auth; else prepare_registry_auth; fi
test "$gar_key" = wif
test "$DOCKER_CONFIG/config.json" = "$HELM_REGISTRY_CONFIG"
test ! -e "$cred_dir/.gar-token"
test -x "$DOCKER_CONFIG/bin/docker-credential-oberth-wif"
printf '%s\n' "$DOCKER_CONFIG" >"$FIXTURE_ROOT/wif-dir"
/usr/bin/python3 -I -S -B - "$HELM_REGISTRY_CONFIG" <<'PY'
import json,sys
with open(sys.argv[1]) as f: config=json.load(f)
assert config == {'credHelpers':{'europe-west4-docker.pkg.dev':'oberth-wif'}}
PY
# A failed credential consumer must abort; the legacy key fixtures all exist.
"$release_image_tool" "$gar_key"
`
			path := filepath.Join(f.root, "wif-auth.sh")
			f.absolute(path, script, 0700)
			f.script = path
			f.env = []string{"OBERTH_RELEASE_WIF=1", "GOOGLE_APPLICATION_CREDENTIALS=/run/oberth-gar/adc.json"}
			f.tool("oberth-release-image", `test "$1" = wif
exit 42`)
			out, err := f.run(action, true)
			if err == nil || f.log() != "" {
				t.Fatalf("failed WIF consumer fell back: %v %s %s", err, out, f.log())
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 42 {
				t.Fatalf("auth config failed before consumer: %v %s", err, out)
			}
			directory, err := os.ReadFile(filepath.Join(f.root, "wif-dir"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(strings.TrimSpace(string(directory))); !os.IsNotExist(err) {
				t.Fatal("public config was not cleaned")
			}
		})
	}
}
