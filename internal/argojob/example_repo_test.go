package argojob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const exampleSecret = "oberth/data/release/example-repo"

func exampleRepoSource(t *testing.T, variant, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "examples", "example-repo", variant, ".oberth", file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func exampleRepoRequest(t *testing.T, trigger argoworkflow.RunTrigger) (Config, Request) {
	t.Helper()
	tier, file := pipeline.TriggerCI, "build.yaml"
	if trigger == argoworkflow.RunTriggerTag {
		tier, file = pipeline.TriggerRelease, "release.yaml"
	}
	r := testRequest(tier, string(exampleRepoSource(t, "v0.22.7", file)))
	r.Repo, r.UpstreamName, r.UpstreamOrg = "example-repo", "forge", "org"
	r.RunTrigger = trigger
	r.SourceVolume = SourceVolume{ClaimName: "example-source", SubPath: "src", VaultCASubPath: vaultCASubPath, BinarySubPath: binarySubPath}
	if tier == pipeline.TriggerRelease {
		// ReceiveHandler.dispatch strips the Git tag namespace before enqueueing.
		r.Ref = strings.TrimPrefix("refs/tags/v0.1.0", "refs/tags/")
		r.ApprovedSecrets = map[string]bool{exampleSecret: true}
		r.StepGrants = map[string]map[string]bool{exampleSecret: {"read-secret": true}}
	}
	c := testConfig()
	c.PerStepIdentities = map[string]PerRepoIdentityConfig{
		"forge/org/example-repo/.oberth/release.yaml/read-secret": {ServiceAccountName: "example-read-secret"},
	}
	return c, r
}

func TestExampleRepoAdmissionAndBuild(t *testing.T) {
	for _, trigger := range []argoworkflow.RunTrigger{argoworkflow.RunTriggerBranch, argoworkflow.RunTriggerPromotion, argoworkflow.RunTriggerTag} {
		t.Run(string(trigger), func(t *testing.T) {
			config, request := exampleRepoRequest(t, trigger)
			authored, err := argoworkflow.Decode(request.Source)
			if err != nil {
				t.Fatal(err)
			}
			if err := argoworkflow.Admit(authored, argoworkflow.Policy{}); err != nil {
				t.Fatal(err)
			}
			built, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			if got := runFacts(t, built)[argoworkflow.TriggerParameter]; got != string(trigger) {
				t.Fatalf("trigger = %s", got)
			}
			if built.Spec.AutomountServiceAccountToken == nil || *built.Spec.AutomountServiceAccountToken {
				t.Fatal("ambient token enabled")
			}
			for _, original := range authored.Spec.Templates {
				actual := exampleRepoTemplate(t, built, original.Name)
				if original.Container == nil {
					continue
				}
				if !reflect.DeepEqual(actual.Container.SecurityContext, original.Container.SecurityContext) {
					t.Errorf("%s security context changed", original.Name)
				}
				if !reflect.DeepEqual(actual.Container.Resources, original.Container.Resources) {
					t.Errorf("%s resources changed", original.Name)
				}
				if len(actual.Sidecars) != len(original.Sidecars) {
					t.Fatal("sidecar removed")
				}
				for i, sidecar := range original.Sidecars {
					got := actual.Sidecars[i]
					if got.Image != sidecar.Image || !reflect.DeepEqual(got.Command, sidecar.Command) || !reflect.DeepEqual(got.Args, sidecar.Args) || !reflect.DeepEqual(got.Resources, sidecar.Resources) || !reflect.DeepEqual(got.SecurityContext, sidecar.SecurityContext) {
						t.Errorf("%s sidecar program/resources/security changed", original.Name)
					}
					assertNoStoreSettings(t, got.Container)
				}
				if !reflect.DeepEqual(actual.Inputs, original.Inputs) || !reflect.DeepEqual(actual.Outputs, original.Outputs) {
					t.Errorf("%s parameter handoff changed", original.Name)
				}
				if original.Container.WorkingDir == SourceMountPath {
					found := false
					for _, mount := range actual.Container.VolumeMounts {
						found = found || mount.MountPath == SourceMountPath && mount.ReadOnly
					}
					if !found {
						t.Errorf("%s missing read-only source", original.Name)
					}
				}
				if original.Name != "read-secret" {
					assertNoStoreSettings(t, *actual.Container)
				}
			}
			if trigger == argoworkflow.RunTriggerTag {
				if got := runFacts(t, built)["oberth-release-tag"]; got != "v0.1.0" {
					t.Fatalf("release tag = %q, want bare tag v0.1.0", got)
				}
				secret := exampleRepoTemplate(t, built, "read-secret")
				if len(secret.Sidecars) != 1 || len(secret.Outputs.Parameters) != 0 || credentialResultReferenced(built, secret) {
					t.Fatal("granted sidecar step must have no path outputs or consumed result")
				}
				if secret.ServiceAccountName != "example-read-secret" {
					t.Fatal("named release grant identity missing")
				}
				mounts := map[string]bool{}
				for _, m := range secret.Container.VolumeMounts {
					mounts[m.MountPath] = true
				}
				for _, want := range []string{SourceMountPath, ReleaseTokenMountPath, SecretsMountPath, VaultCAMountPath, OberthBinMountPath} {
					if !mounts[want] {
						t.Errorf("granted step missing %s", want)
					}
				}
			} else {
				if built.Spec.OnExit != "cleanup" {
					t.Fatal("cancel exit handler missing")
				}
				testStep := exampleRepoTemplate(t, authored, "test")
				if !assistHoldEligible(testStep, "") {
					t.Fatal("test cannot exercise assisted continuation")
				}
			}
		})
	}
}

func exampleRepoTemplate(t *testing.T, workflow *wfv1.Workflow, name string) *wfv1.Template {
	t.Helper()
	for i := range workflow.Spec.Templates {
		if workflow.Spec.Templates[i].Name == name {
			return &workflow.Spec.Templates[i]
		}
	}
	t.Fatalf("missing template %s", name)
	return nil
}

func TestExampleRepoRefusals(t *testing.T) {
	t.Run("ungranted-secret", func(t *testing.T) {
		config, request := exampleRepoRequest(t, argoworkflow.RunTriggerTag)
		request.StepGrants = map[string]map[string]bool{exampleSecret: {"report": true}}
		if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "secret grant denied") {
			t.Fatalf("wrong-step grant: %v", err)
		}
	})
	for _, shape := range []string{"path-output", "consumed-result"} {
		t.Run("granted-sidecar-"+shape, func(t *testing.T) {
			config, request := exampleRepoRequest(t, argoworkflow.RunTriggerTag)
			document, err := argoworkflow.Decode(request.Source)
			if err != nil {
				t.Fatal(err)
			}
			secret := exampleRepoTemplate(t, document, "read-secret")
			output := `path-read output parameter "marker"`
			if shape == "path-output" {
				secret.Outputs.Parameters = []wfv1.Parameter{{Name: "marker", ValueFrom: &wfv1.ValueFrom{Path: "/tmp/marker"}}}
			} else {
				parent := exampleRepoTemplate(t, document, "release")
				parent.Outputs.Parameters = []wfv1.Parameter{{Name: "marker", ValueFrom: &wfv1.ValueFrom{Parameter: "{{tasks.read-secret.outputs.result}}"}}}
				output = "referenced outputs.result"
			}
			// Structural admission succeeds; Build checks the actual grant and Pod.
			if err := argoworkflow.Admit(document, argoworkflow.Policy{}); err != nil {
				t.Fatal(err)
			}
			request.Source, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			want := `argojob: credentialed template "read-secret" cannot use ` + output + ` because another container of the Pod can redirect what Argo reads`
			if _, err := Build(config, request); err == nil || err.Error() != want {
				t.Fatalf("granted sidecar refusal = %v, want %s", err, want)
			}
		})
	}
	for _, name := range []string{"server-owned-parameter", "host-path"} {
		t.Run(name, func(t *testing.T) {
			config, request := exampleRepoRequest(t, argoworkflow.RunTriggerBranch)
			document, err := argoworkflow.Decode(request.Source)
			if err != nil {
				t.Fatal(err)
			}
			want := "server-owned"
			if name == "host-path" {
				document.Spec.Volumes = append(document.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
				want = "node filesystem"
			} else {
				document.Spec.Arguments.Parameters = append(document.Spec.Arguments.Parameters, wfv1.Parameter{Name: "oberth-sha", Value: wfv1.AnyStringPtr("forged")})
			}
			request.Source, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := argoworkflow.Admit(document, argoworkflow.Policy{}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("admission: %v", err)
			}
			if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Build: %v", err)
			}
		})
	}
}

func TestExampleRepoShellProject(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ready") }))
	defer fixture.Close()
	for _, variant := range []string{"v0.22.7", "next"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(repositoryRoot(t), "examples", "example-repo", variant)
			for _, name := range []string{"app.sh", "test.sh", "expect.txt"} {
				data, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			run := func() ([]byte, error) {
				cmd := exec.Command("sh", "./test.sh", fixture.URL)
				cmd.Dir = dir
				return cmd.CombinedOutput()
			}
			if out, err := run(); err != nil || !bytes.Contains(out, []byte("test: sidecar passed")) {
				t.Fatalf("project: %s, %v", out, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "expect.txt"), []byte("broken\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := run(); err == nil {
				t.Fatal("broken source accepted")
			}
		})
	}
}
