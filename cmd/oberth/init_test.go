package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestInitFreshGoProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\ngo 1.22\n")

	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".oberth", "build.yaml")); err != nil {
		t.Fatalf("build.yaml not created: %v", err)
	}
	if !strings.Contains(output.String(), "go") || !strings.Contains(output.String(), "go.mod") {
		t.Fatalf("output = %q, want Go detection with go.mod", output.String())
	}
}

func TestInitPrecedenceGoOverNode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}\n")

	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "detected: go") {
		t.Fatalf("output = %q, want Go detection", output.String())
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(oberthDir, "build.yaml"), "existing")
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	err := executeInit(dir, "", false, &output)
	if err == nil {
		t.Fatal("expected refusal error")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v, want refusal with --force hint", err)
	}
}

func TestInitForcePreservesSiblings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(oberthDir, "build.yaml"), "old")
	writeTestFile(t, filepath.Join(oberthDir, "other.txt"), "sibling")
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	if err := executeInit(dir, "", true, &output); err != nil {
		t.Fatal(err)
	}
	sibling, err := os.ReadFile(filepath.Join(oberthDir, "other.txt"))
	if err != nil || string(sibling) != "sibling" {
		t.Fatalf("sibling file modified: %v %q", err, sibling)
	}
}

func TestInitTypeOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "go", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--type go") {
		t.Fatalf("output = %q, want --type go", output.String())
	}
}

func TestInitTypeDeprecationNotice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "node", false, &output); err != nil {
		t.Fatal(err)
	}
	out := output.String()
	if !strings.Contains(out, "--type has no effect yet") {
		t.Fatalf("output missing deprecation notice: %q", out)
	}
}

func TestInitInvalidType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	err := executeInit(dir, "rust", false, &output)
	if err == nil || !strings.Contains(err.Error(), "unknown project type") {
		t.Fatalf("error = %v, want unknown project type", err)
	}
}

func TestInitPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 0600", perm)
	}
}

func TestInitWritesValidArgoWorkflowYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "apiVersion: argoproj.io/v1alpha1") {
		t.Fatal("generated build.yaml missing Argo apiVersion")
	}
	if !strings.Contains(text, "kind: Workflow") {
		t.Fatal("generated build.yaml missing kind: Workflow")
	}
	if !strings.Contains(text, "oberth.ci/size:") {
		t.Fatal("generated build.yaml missing size annotation")
	}
	if !strings.Contains(text, "entrypoint: ci") {
		t.Fatal("generated build.yaml missing entrypoint")
	}
}

func TestUsageListsInit(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runCLI(context.Background(), nil, nil, &output)
	if err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("usage error = %v, want 'init' in message", err)
	}
}

func TestInitSummaryOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	out := output.String()
	if !strings.Contains(out, "wrote: .oberth/build.yaml") {
		t.Fatalf("output missing 'wrote' line: %q", out)
	}
	if !strings.Contains(out, "5 steps, 4 dependencies") {
		t.Fatalf("output missing correct step/dependency count: %q", out)
	}
	if !strings.Contains(out, "fetch") || !strings.Contains(out, "report") {
		t.Fatalf("output missing DAG step names: %q", out)
	}
}

func TestInitDAGDiagramInOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "generic", false, &output); err != nil {
		t.Fatal(err)
	}
	out := output.String()
	for _, step := range []string{"fetch", "analyze", "validate", "report", "notify"} {
		if !strings.Contains(out, step) {
			t.Errorf("DAG diagram missing step %q", step)
		}
	}
	if !strings.Contains(out, "DAG:") {
		t.Errorf("output missing DAG label")
	}
}

func TestInitAllTypesGenerateSameTemplate(t *testing.T) {
	t.Parallel()
	var contents []string
	for _, projType := range allProjectTypes {
		dir := t.TempDir()
		var output bytes.Buffer
		if err := executeInit(dir, string(projType), false, &output); err != nil {
			t.Fatalf("init %s: %v", projType, err)
		}
		content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, string(content))
	}
	for i := 1; i < len(contents); i++ {
		if contents[i] != contents[0] {
			t.Fatalf("template for %s differs from %s", allProjectTypes[i], allProjectTypes[0])
		}
	}
}

func TestInitAllTemplatesUseAllowlistedImages(t *testing.T) {
	t.Parallel()
	for _, projType := range allProjectTypes {
		t.Run(string(projType), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var output bytes.Buffer
			if err := executeInit(dir, string(projType), false, &output); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			wf, err := argoworkflow.Decode(content)
			if err != nil {
				t.Fatalf("decode build.yaml for %s: %v", projType, err)
			}
			// The generated template must pass admission against the code-
			// default allowlist -- no bespoke policy needed.
			if err := argoworkflow.Admit(wf, argoworkflow.Policy{}); err != nil {
				t.Fatalf("admission rejected %s template against code defaults: %v", projType, err)
			}
		})
	}
}

// TestInitAllTemplatesHaveDigestPinnedImages proves all project types
// generate templates with digest-pinned images (tag@sha256:...) so the
// mandatory-digest admission does not reject them on first push.
func TestInitAllTemplatesHaveDigestPinnedImages(t *testing.T) {
	t.Parallel()
	for _, projType := range []string{"go", "node", "python", "generic"} {
		t.Run(projType, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if projType == "go" {
				writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\ngo 1.22\n")
			}
			var output bytes.Buffer
			if err := executeInit(dir, projType, false, &output); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(content)
			if !strings.Contains(text, "@sha256:") {
				t.Fatalf("generated YAML for %s has no digest-pinned image", projType)
			}
			// Verify no all-zeros sentinel digest remains.
			if strings.Contains(text, "sha256:00000000") {
				t.Fatalf("generated YAML for %s still contains all-zeros sentinel digest", projType)
			}
			// Verify the refresh comment is present.
			if !strings.Contains(text, "Refresh digest: crane digest") {
				t.Fatalf("generated YAML for %s missing digest refresh comment", projType)
			}
		})
	}
}

func TestInitNoAllowlistWarning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "generic", false, &output); err != nil {
		t.Fatal(err)
	}
	out := output.String()
	// The template uses images that are in the default allowlist, so no
	// allowlist-extension warning should appear.
	if strings.Contains(out, "WARNING") {
		t.Fatalf("unexpected WARNING in output: %q", out)
	}
}

func TestInitDemoTemplatePassesAdmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "generic", false, &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := argoworkflow.Decode(content)
	if err != nil {
		t.Fatalf("decode build.yaml: %v", err)
	}
	// (a) Empty policy triggers the code-default fallback.
	if err := argoworkflow.Admit(wf, argoworkflow.Policy{}); err != nil {
		t.Fatalf("admission rejected build.yaml against code defaults: %v", err)
	}
	// (b) Exact chart default from values.yaml.
	if err := argoworkflow.Admit(wf, argoworkflow.Policy{RunnerImagePrefixes: []string{"golang:", "debian:", "aquasec/trivy:"}}); err != nil {
		t.Fatalf("admission rejected build.yaml against chart defaults: %v", err)
	}
}

func TestInitDemoDAGStepNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "generic", false, &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, step := range []string{"fetch", "analyze", "validate", "report", "notify"} {
		if !strings.Contains(text, "name: "+step) {
			t.Errorf("generated YAML missing step %q", step)
		}
		if !strings.Contains(text, "template: "+step) {
			t.Errorf("generated YAML missing template reference for %q", step)
		}
	}
}

func TestInitDetectedLanguageDemoSuffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		projType   string
		markerFile string
		markerData string
		wantSuffix bool
	}{
		{"go", "go.mod", "module test\ngo 1.22\n", true},
		{"node", "package.json", "{}\n", true},
		{"python", "pyproject.toml", "[project]\nname = \"test\"\n", true},
		{"generic", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.projType, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.markerFile != "" {
				writeTestFile(t, filepath.Join(dir, tc.markerFile), tc.markerData)
			}
			var output bytes.Buffer
			if err := executeInit(dir, "", false, &output); err != nil {
				t.Fatal(err)
			}
			out := output.String()
			hasSuffix := strings.Contains(out, "generic demo pipeline generated")
			if tc.wantSuffix && !hasSuffix {
				t.Fatalf("output for %s missing demo suffix: %q", tc.projType, out)
			}
			if !tc.wantSuffix && hasSuffix {
				t.Fatalf("output for %s has unexpected demo suffix: %q", tc.projType, out)
			}
			if tc.wantSuffix && !strings.Contains(out, "customize for your "+tc.projType+" project") {
				t.Fatalf("output for %s missing project-specific customize hint: %q", tc.projType, out)
			}
		})
	}
}

func TestInitRejectsOberthDirSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	externalDir := t.TempDir()
	if err := os.Symlink(externalDir, filepath.Join(dir, ".oberth")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	err := executeInit(dir, "", false, &output)
	if err == nil {
		t.Fatal("expected error for symlinked .oberth directory")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

func TestInitRejectsBuildYAMLSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(oberthDir, "build.yaml")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	err := executeInit(dir, "", true, &output)
	if err == nil {
		t.Fatal("expected error for symlinked build.yaml")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

func TestInitRejectsDanglingSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nonexistent"), filepath.Join(dir, ".oberth")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	err := executeInit(dir, "", false, &output)
	if err == nil {
		t.Fatal("expected error for dangling .oberth symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

// TestDefaultRunnerImagePrefixesMatchChart parses the chart values.yaml and
// asserts that its runnerImagePrefixes list equals the code-level default in
// periapsis.DefaultRunnerImagePrefixes. Drift between the two is a silent
// admission mismatch: the server's flag default may disagree with the chart's
// rendered deployment args, causing `oberth init` templates to pass one gate
// and fail the other.
func TestDefaultRunnerImagePrefixesMatchChart(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join("..", "..", "charts", "oberth", "values.yaml"))
	if err != nil {
		t.Skipf("chart values.yaml not found: %v", err)
	}
	var values struct {
		RunnerImagePrefixes []string `json:"runnerImagePrefixes"`
	}
	if err := yaml.Unmarshal(content, &values); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	if len(values.RunnerImagePrefixes) != len(periapsis.DefaultRunnerImagePrefixes) {
		t.Fatalf("chart runnerImagePrefixes %v != code DefaultRunnerImagePrefixes %v",
			values.RunnerImagePrefixes, periapsis.DefaultRunnerImagePrefixes)
	}
	for i, prefix := range values.RunnerImagePrefixes {
		if prefix != periapsis.DefaultRunnerImagePrefixes[i] {
			t.Fatalf("chart runnerImagePrefixes[%d] = %q, code DefaultRunnerImagePrefixes[%d] = %q",
				i, prefix, i, periapsis.DefaultRunnerImagePrefixes[i])
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
