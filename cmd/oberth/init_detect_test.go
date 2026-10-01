package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectProjectNodeByPackageJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}\n")
	detected, reason := detectProject(dir)
	if detected != projectNode || !strings.Contains(reason, "package.json") {
		t.Fatalf("detect = %s (%s), want node", detected, reason)
	}
}

func TestDetectProjectPythonByPyprojectTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\nname = \"test\"\n")
	detected, reason := detectProject(dir)
	if detected != projectPython || !strings.Contains(reason, "pyproject.toml") {
		t.Fatalf("detect = %s (%s), want python", detected, reason)
	}
}

func TestDetectProjectPythonBySetupPy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "setup.py"), "from setuptools import setup\nsetup()\n")
	detected, reason := detectProject(dir)
	if detected != projectPython || !strings.Contains(reason, "setup.py") {
		t.Fatalf("detect = %s (%s), want python", detected, reason)
	}
}

func TestDetectProjectGenericFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	detected, reason := detectProject(dir)
	if detected != projectGeneric || !strings.Contains(reason, "no recognized") {
		t.Fatalf("detect = %s (%s), want generic", detected, reason)
	}
}

func TestDetectProjectGoTakesPrecedenceOverNode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module test\n")
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}\n")
	detected, _ := detectProject(dir)
	if detected != projectGo {
		t.Fatalf("go/node precedence: detected %s", detected)
	}
}

func TestDetectProjectNodeTakesPrecedenceOverPython(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}\n")
	writeTestFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\n")
	detected, _ := detectProject(dir)
	if detected != projectNode {
		t.Fatalf("node/python precedence: detected %s", detected)
	}
}

func TestExecuteInitNodeAutoDetect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), "{}\n")
	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "detected: node") {
		t.Fatalf("output = %q, want node detection", output.String())
	}
	content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// All types generate the same demo template with a debian runner image.
	if !strings.Contains(string(content), "debian:trixie-slim") {
		t.Fatal("generated YAML missing debian image reference")
	}
}

func TestExecuteInitPythonAutoDetect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\nname = \"test\"\n")
	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "detected: python") {
		t.Fatalf("output = %q, want python detection", output.String())
	}
}

func TestExecuteInitGenericAutoDetect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var output bytes.Buffer
	if err := executeInit(dir, "", false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "detected: generic") {
		t.Fatalf("output = %q, want generic detection", output.String())
	}
}
