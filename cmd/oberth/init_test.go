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
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
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
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
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
	err := executeInit(context.Background(), dir, "", "", false, &output)
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
	if err := executeInit(context.Background(), dir, "", "", true, &output); err != nil {
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
	if err := executeInit(context.Background(), dir, "go", "", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "detected: go (--type go)") {
		t.Fatalf("output = %q, want the override named as the reason", output.String())
	}
}

func TestInitInvalidType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	err := executeInit(context.Background(), dir, "erlang", "", false, &output)
	if err == nil || !strings.Contains(err.Error(), "unknown project type") {
		t.Fatalf("error = %v, want unknown project type", err)
	}
}

func TestInitPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
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
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
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
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
		t.Fatal(err)
	}
	out := output.String()
	if !strings.Contains(out, "wrote: .oberth/build.yaml") {
		t.Fatalf("output missing 'wrote' line: %q", out)
	}
	// The count is counted from the document that was written. The summary
	// this replaces claimed "5 steps, 3 dependencies, ~30 seconds" for every
	// repository, which was true of the demo and of nothing else.
	if !strings.Contains(out, "4 steps") {
		t.Fatalf("output missing the real step count: %q", out)
	}
	if !strings.Contains(out, "copy-source -> vet -> test -> build") {
		t.Fatalf("output missing the real chain: %q", out)
	}
}

// TestInitChainIsSequentialNotADAG guards the ordering decision. Argo lets the
// siblings of a failed DAG task run to completion, so a red early step would
// keep a long test suite running and report minutes after it knew the answer.
func TestInitChainIsSequentialNotADAG(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")

	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml")) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "dag:") {
		t.Fatalf("generated pipeline must chain its steps, not fan them out:\n%s", text)
	}
	if !strings.Contains(text, "steps:") {
		t.Fatalf("generated pipeline has no step chain:\n%s", text)
	}
	for _, step := range []string{"copy-source", "vet", "test", "build"} {
		if !strings.Contains(text, "name: "+step) {
			t.Errorf("generated YAML missing step %q", step)
		}
		if !strings.Contains(text, "template: "+step) {
			t.Errorf("generated YAML missing template reference for %q", step)
		}
	}
}

// TestInitEachTypeGeneratesItsOwnTemplate is the inversion of the test it
// replaces, which asserted every project type produced the SAME document.
// That was true, and it was the defect: the document was a demo that had
// nothing to do with any of them.
func TestInitEachTypeGeneratesItsOwnTemplate(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, projType := range []string{"go", "node", "maven", "generic"} {
		dir := t.TempDir()
		var output bytes.Buffer
		if err := executeInit(context.Background(), dir, projType, "", false, &output); err != nil {
			t.Fatalf("init %s: %v", projType, err)
		}
		content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml")) // #nosec G304 -- test temp dir
		if err != nil {
			t.Fatal(err)
		}
		for other, text := range seen {
			if text == string(content) {
				t.Fatalf("%s and %s generated an identical pipeline", projType, other)
			}
		}
		seen[projType] = string(content)
	}
}

func TestInitAllTemplatesUseAllowlistedImages(t *testing.T) {
	t.Parallel()
	for _, projType := range []string{"go", "node", "maven", "generic"} {
		t.Run(projType, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var output bytes.Buffer
			if err := executeInit(context.Background(), dir, projType, "", false, &output); err != nil {
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
	for _, projType := range []string{"go", "node", "maven", "generic"} {
		t.Run(projType, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if projType == "go" {
				writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\ngo 1.22\n")
			}
			var output bytes.Buffer
			if err := executeInit(context.Background(), dir, projType, "", false, &output); err != nil {
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
	if err := executeInit(context.Background(), dir, "generic", "", false, &output); err != nil {
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
	if err := executeInit(context.Background(), dir, "generic", "", false, &output); err != nil {
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

// TestInitNamesWhatItDetectedAndWhy replaces the test that asserted every
// recognized language got the suffix "generating demo pipeline". There is no
// demo any more, so what the line has to carry is the kind and the evidence.
func TestInitNamesWhatItDetectedAndWhy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind       string
		markerFile string
		markerData string
		wantSource string
	}{
		{"go", "go.mod", "module test\ngo 1.22\n", "go.mod found"},
		{"node", "package.json", `{"scripts":{"test":"vitest"}}` + "\n", "package.json"},
		{"maven", "pom.xml", "<project><artifactId>svc</artifactId></project>\n", "pom.xml found"},
		{"generic", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.markerFile != "" {
				writeTestFile(t, filepath.Join(dir, tc.markerFile), tc.markerData)
			}
			var output bytes.Buffer
			if err := executeInit(context.Background(), dir, "", "", false, &output); err != nil {
				t.Fatal(err)
			}
			out := output.String()
			if !strings.Contains(out, "detected: "+tc.kind) {
				t.Fatalf("output for %s does not name the kind: %q", tc.kind, out)
			}
			if tc.wantSource != "" && !strings.Contains(out, tc.wantSource) {
				t.Fatalf("output for %s does not name its evidence %q: %q", tc.kind, tc.wantSource, out)
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
	err := executeInit(context.Background(), dir, "", "", false, &output)
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
	err := executeInit(context.Background(), dir, "", "", true, &output)
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
	err := executeInit(context.Background(), dir, "", "", false, &output)
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
