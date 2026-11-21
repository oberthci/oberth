package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestValidateUsesPushAdmissionOffline(t *testing.T) {
	for _, scenario := range []struct{ name, before, after string }{
		{"pod-patch", "spec:\n", "spec:\n  podSpecPatch: '{}'\n"},
		{"host-network", "spec:\n", "spec:\n  hostNetwork: true\n"},
		{"reserved-label", "metadata:\n", "metadata:\n  labels: {oberth.ci/managed: injected}\n"},
		{"reserved-parameter", "spec:\n", "spec:\n  arguments:\n    parameters: [{name: oberth-credential, value: injected}]\n"},
		{"http-template", "      container:\n", "      http: {url: 'https://oberth.ci'}\n      container:\n"},
		{"plugin-template", "      container:\n", "      plugin: {name: injected}\n      container:\n"},
		{"native-argument", "spec:\n", "spec:\n  arguments:\n    parameters: [{name: missing-value}]\n"},
		{"second-document", "", "---\nkind: Workflow\n"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app, _, diagnostic := fixture(t, fake.Bindings())
			app.directory = t.TempDir()
			app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("offline validate contacted the server")
				return nil, nil
			})
			body := strings.Replace(starterWorkflow(), scenario.before, scenario.after, 1)
			if scenario.before == "" {
				body = starterWorkflow() + scenario.after
			}
			if err := os.WriteFile(filepath.Join(app.directory, "workflow.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if app.run(t.Context(), []string{"validate", "workflow.yaml"}) != 1 {
				t.Fatal("push refusal accepted offline", diagnostic.String())
			}
		})
	}
}

func TestValidatePrintsSharedPlan(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	if err := os.WriteFile(filepath.Join(app.directory, "workflow.yaml"), []byte(starterWorkflow()), 0600); err != nil {
		t.Fatal(err)
	}
	if app.run(t.Context(), []string{"validate", "workflow.yaml", "--json"}) != 0 {
		t.Fatal(diagnostic.String())
	}
	var result apicontract.RunCheckResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || result.Plan == nil || !result.Plan.Exact || len(result.Plan.Entries) != 1 || result.Plan.Entries[0].Name != "build" {
		t.Fatal("validate omitted the shared offline plan", out.String(), err)
	}
}

func TestMountedFilePathsAreRequiredAndWitnessIsUnknown(t *testing.T) {
	for _, scenario := range []string{"missing", "relative", "witness"} {
		t.Run(scenario, func(t *testing.T) {
			var object map[string]any
			if err := json.Unmarshal(validConfig(), &object); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				delete(object, "files")
			case "relative":
				object["files"].(map[string]any)["ssh_host_key"] = "key"
			case "witness":
				object["witness"] = map[string]any{}
			}
			raw, _ := json.Marshal(object)
			if _, err := decodeConfig(raw); err == nil {
				t.Fatal("hidden mount default or witness config accepted")
			}
		})
	}
}

func TestServerReadsConfiguredMountPaths(t *testing.T) {
	f := fixtureServer(t)
	for _, path := range []*string{&f.config.Files.CACertificate, &f.config.Files.CAPrivateKey, &f.config.Files.AdminPublicKey, &f.config.Files.AdminPushKey} {
		configured := filepath.Join(t.TempDir(), "chosen-mounted-file")
		if err := os.Rename(*path, configured); err != nil {
			t.Fatal(err)
		}
		*path = configured
	}
	identity, _, err := readMounts(f.config, f.deps)
	if err != nil || identity.certPath != "files.ca_certificate" || identity.keyPath != "files.ca_private_key" {
		t.Fatal("configured mounted paths were ignored", err)
	}
}

func TestExecutorAccountIsNotConfigured(t *testing.T) {
	if _, err := decodeRuntime([]byte(`{"executor_account":"external"}`)); err == nil {
		t.Fatal("obsolete executor account setting accepted")
	}
}

func TestRefusalShowsContextBeforeOptionalDetail(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	data := json.RawMessage(`{"reason":"Connection was refused.","remedy":"Confirm the endpoint and retry.","context":["Public connection context."],"detail":"More public detail."}`)
	if err := app.render(data, true, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostic.String(), "Public connection context.") || strings.Contains(diagnostic.String(), "More public detail.") {
		t.Fatal("default refusal context changed", diagnostic.String())
	}
	diagnostic.Reset()
	if err := app.render(data, true, commonOptions{detail: true}); err != nil || !strings.Contains(diagnostic.String(), "More public detail.") {
		t.Fatal("explicit detail was hidden", err, diagnostic.String())
	}
}

func TestValidateDefersSecretStoreChecksUntilPush(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("offline validate contacted the server")
		return nil, nil
	})
	body := strings.Replace(starterWorkflow(), "    - name: build\n", "    - name: build\n      metadata:\n        annotations: {oberth.ci/secret-paths: public/key}\n", 1)
	body = strings.Replace(body, "command: [sh, -c]", "command: [oberth, secretstore, exec, --, sh, -c]", 1)
	if err := os.WriteFile(filepath.Join(app.directory, "workflow.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"validate", "workflow.yaml"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	if !strings.Contains(out.String(), "Needs before the first run:") || !strings.Contains(out.String(), "oberth secretstore check") || strings.Contains(diagnostic.String(), "Secret store is not configured") {
		t.Fatal(out.String(), diagnostic.String())
	}
}
