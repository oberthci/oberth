package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepositoryBuildYAMLPasses(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// Oberth's own pipelines use golang: and aquasec/trivy: images.
	if err := runValidate(context.Background(), []string{"--runner-image-prefixes=golang:,aquasec/trivy:", root}, &output); err != nil {
		t.Fatalf("validate %s: %v\n%s", root, err, output.String())
	}
	text := output.String()
	for _, expected := range []string{
		"build.yaml",
		"ok  YAML decode (strict)",
		"ok  admission (ci trigger)",
		"result: PASS",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("output missing %q:\n%s", expected, text)
		}
	}
}

func TestValidateMissingBuildYAMLFails(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{t.TempDir()}, &output)
	if err == nil {
		t.Fatalf("missing build.yaml accepted:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "not found") {
		t.Fatalf("output does not mention missing file:\n%s", output.String())
	}
}

func TestValidateUsageErrors(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{
		{"one", "two"},
		{"--unknown-flag"},
	} {
		var output bytes.Buffer
		err := runValidate(context.Background(), arguments, &output)
		if !errors.Is(err, errUsage) {
			t.Errorf("runValidate(%q) = %v, want usage error", arguments, err)
		}
	}
}

func TestResolveValidateTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// From repository root
	target, err := resolveValidateTarget(root)
	if err != nil {
		t.Fatalf("resolveValidateTarget(%q): %v", root, err)
	}
	if target.repoRoot != root {
		t.Fatalf("resolveValidateTarget(%q) = %+v, want repoRoot=%s", root, target, root)
	}

	// From .oberth directory
	target, err = resolveValidateTarget(oberthDir)
	if err != nil {
		t.Fatalf("resolveValidateTarget(%q): %v", oberthDir, err)
	}
	if target.repoRoot != root {
		t.Fatalf("resolveValidateTarget(%q) = %+v, want repoRoot=%s", oberthDir, target, root)
	}

	// Nonexistent target
	if _, err := resolveValidateTarget(filepath.Join(root, "absent")); err == nil {
		t.Fatal("nonexistent target resolved")
	}
}

func TestValidateRejectsExternalSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(oberthDir, "build.yaml")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{root}, &output)
	if err == nil {
		t.Fatalf("external symlink accepted:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "symlink") {
		t.Fatalf("output does not mention symlink:\n%s", output.String())
	}
}

func TestValidateRejectsInternalSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "go.mod"), filepath.Join(oberthDir, "build.yaml")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{root}, &output)
	if err == nil {
		t.Fatalf("internal symlink accepted:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "symlink") {
		t.Fatalf("output does not mention symlink:\n%s", output.String())
	}
}

func TestValidateRejectsTooLargeFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	large := make([]byte, 1<<20+1)
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{root}, &output)
	if err == nil {
		t.Fatalf("overlarge file accepted:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "exceeds") {
		t.Fatalf("output does not mention size limit:\n%s", output.String())
	}
}

// TestValidateExamplesPassAdmission validates every example in the
// examples/ directory passes admission with the default runner image prefixes.
func TestValidateExamplesPassAdmission(t *testing.T) {
	t.Parallel()
	examplesRoot, err := filepath.Abs(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(examplesRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		examplePath := filepath.Join(examplesRoot, entry.Name())
		if _, err := os.Stat(filepath.Join(examplePath, ".oberth")); err != nil {
			continue // not an example with a pipeline
		}
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := runValidate(context.Background(), []string{examplePath}, &output); err != nil {
				t.Fatalf("validate %s: %v\n%s", entry.Name(), err, output.String())
			}
			text := output.String()
			if !strings.Contains(text, "result: PASS") {
				t.Fatalf("example %s did not pass:\n%s", entry.Name(), text)
			}
		})
	}
}

// TestReleaseExampleArtifactsOnSharedVolume validates that the go-project
// release example writes build artifacts to a shared volume (not /tmp),
// and that the envconsul config file exists.
func TestReleaseExampleArtifactsOnSharedVolume(t *testing.T) {
	t.Parallel()
	releaseYAML, err := os.ReadFile(filepath.Join("..", "..", "examples", "go-project", ".oberth", "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(releaseYAML)
	// Build output must go to the shared release subPath, not /tmp.
	if strings.Contains(text, "-o\", \"/tmp/app-linux-amd64") {
		t.Error("release-build still writes to /tmp (per-pod); must use shared volume")
	}
	if !strings.Contains(text, "subPath: release") {
		t.Error("release templates missing release subPath mount")
	}
	// The envconsul config file must exist.
	if _, err := os.Stat(filepath.Join("..", "..", "examples", "go-project", ".oberth", "envconsul.hcl")); err != nil {
		t.Errorf("envconsul.hcl missing: %v", err)
	}
	// Cosign must be installed (not just invoked).
	if !strings.Contains(text, "fetch-cosign") || !strings.Contains(text, "verify-cosign") || !strings.Contains(text, "install-cosign") {
		t.Error("release example missing cosign install steps (fetch/verify/install)")
	}
	// Cosign pin file must exist.
	if _, err := os.Stat(filepath.Join("..", "..", "examples", "go-project", ".oberth", "pins", "cosign.sha256")); err != nil {
		t.Errorf("cosign.sha256 pin missing: %v", err)
	}
	// Uploads must use immutable-create (If-None-Match: *) to prevent silent
	// overwrites of already-published artifacts (issue #91).
	if !strings.Contains(text, "If-None-Match") {
		t.Error("release example missing If-None-Match header for immutable-create uploads")
	}
	// cosign.pub must be published alongside signed artifacts so downstream
	// verification is self-contained per release (issue #92).
	if !strings.Contains(text, "cosign.pub") {
		t.Error("release example missing cosign.pub publication")
	}
	// The release tag must be read via shell parameter expansion
	// (${OBERTH_RELEASE_TAG...}), never Kubernetes $(VAR) substitution nor an
	// Argo {{workflow.parameters.*}} template. Both $(VAR) and {{...}} are
	// text-substituted into the /bin/sh -c script SOURCE (by kubelet and Argo
	// respectively) and re-parsed by the shell, so a crafted tag name would
	// execute inside this credentialed release step (CWE-78, issue #94).
	// ${OBERTH_RELEASE_TAG} is expanded by the shell as inert data.
	if strings.Contains(text, "$(OBERTH_RELEASE_TAG)") {
		t.Error("release example uses $(OBERTH_RELEASE_TAG): kubelet text-substitutes it into the shell body (injectable); use ${OBERTH_RELEASE_TAG} shell expansion (#94)")
	}
	if strings.Contains(text, "{{workflow.parameters.") {
		t.Error("release example interpolates {{workflow.parameters.*}} into a shell body (injectable); use ${OBERTH_RELEASE_TAG} shell expansion (#94)")
	}
	if !strings.Contains(text, "${OBERTH_RELEASE_TAG") {
		t.Error("release example no longer reads ${OBERTH_RELEASE_TAG}; the tag must remain wired to the publish URL via safe shell expansion (#94)")
	}
}

func TestValidateWithValidBuildYAML(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(buildYAMLDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{root}, &output); err != nil {
		t.Fatalf("valid build.yaml rejected: %v\n%s", err, output.String())
	}
	text := output.String()
	if !strings.Contains(text, "result: PASS") {
		t.Fatalf("output missing PASS:\n%s", text)
	}
}
