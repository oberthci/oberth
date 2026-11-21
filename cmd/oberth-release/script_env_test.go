package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// inheritedRelease is what a pipeline step hands the test process: the
// release variables of .oberth/build.yaml and the VERSION that make exports.
func inheritedRelease() []string {
	return []string{"RELEASE_BASE", "RELEASE_DIR", "RELEASE_SHARED", "VERSION"}
}

// TestMain runs the whole package with the release variables pointing at an
// existing directory, as the pipeline's test step does, so a script test that
// reads the caller's release values instead of its own fails on every host.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "inherited-release-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "inherited release directory:", err)
		os.Exit(1)
	}
	for _, name := range inheritedRelease() {
		if err = os.Setenv(name, dir); err != nil {
			fmt.Fprintln(os.Stderr, "inherited release variable:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if err = os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "inherited release directory:", err)
		code = 1
	}
	os.Exit(code)
}

// scriptEnv is the environment for a release script under test: the caller's
// environment without any release variable, then the test's own values.
func scriptEnv(values ...string) []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name != "VERSION" && !strings.HasPrefix(name, "RELEASE_") {
			env = append(env, entry)
		}
	}
	return append(env, values...)
}

func TestSuiteRunsWithInheritedReleaseVariables(t *testing.T) {
	for _, name := range inheritedRelease() {
		info, err := os.Stat(os.Getenv(name))
		if err != nil || !info.IsDir() {
			t.Fatal("suite does not run with an inherited release directory", name, err)
		}
	}
	for _, entry := range scriptEnv("RELEASE_DIR=own") {
		name, value, _ := strings.Cut(entry, "=")
		if (name == "VERSION" || strings.HasPrefix(name, "RELEASE_")) && entry != "RELEASE_DIR=own" {
			t.Fatal("script environment kept an inherited release variable", name, value)
		}
	}
}
