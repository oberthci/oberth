package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeResolver returns a kubeResolver that reports a fixed target with
// optional extra helm args. Tests that need --kubeconfig can pass args.
func fakeResolver(target string, args ...string) kubeResolver {
	return func(_ string) ([]string, string, error) {
		return args, target, nil
	}
}

func TestPreflightDrift_EmptyValues(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return []byte(""), nil // empty values document
		}
		return nil, fmt.Errorf("unexpected helm call: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default"))
	if err == nil {
		t.Fatal("expected error for empty values document")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention empty values: %v", err)
	}
}

func TestPreflightDrift_WhitespaceOnlyValues(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return []byte("   \n\t  \n"), nil
		}
		return nil, fmt.Errorf("unexpected helm call: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default"))
	if err == nil {
		t.Fatal("expected error for whitespace-only values")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention empty values: %v", err)
	}
}

func TestPreflightDrift_MissingRelease(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return nil, fmt.Errorf("Error: release: not found")
		}
		return nil, fmt.Errorf("unexpected helm call: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default"))
	if err == nil {
		t.Fatal("expected error for missing release")
	}
	if !strings.Contains(err.Error(), "context default") {
		t.Fatalf("error should name the target: %v", err)
	}
}

func TestPreflightDrift_TemplateError(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return []byte("replicaCount: 1\nimage:\n  tag: v0.15.0\n"), nil
		}
		if args[0] == "template" {
			return nil, fmt.Errorf("Error: template: chart/templates/deployment.yaml:42: required value \"service.httpsPort\" not set")
		}
		return nil, fmt.Errorf("unexpected helm call: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default"))
	if err == nil {
		t.Fatal("expected error when template fails")
	}
	if !strings.Contains(err.Error(), "chart template failed") {
		t.Fatalf("error should report template failure: %v", err)
	}
	if !strings.Contains(err.Error(), "context default") {
		t.Fatalf("error should name the target: %v", err)
	}
}

func TestPreflightDrift_Success(t *testing.T) {
	t.Parallel()
	var capturedTemplateArgs []string
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return []byte("replicaCount: 1\nimage:\n  tag: v0.15.0\n"), nil
		}
		if args[0] == "template" {
			capturedTemplateArgs = args
			return []byte("---\napiVersion: v1\nkind: ConfigMap\n"), nil
		}
		return nil, fmt.Errorf("unexpected helm call: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("output should contain OK: %s", out.String())
	}
	// Verify the template command used the default chart and release.
	if len(capturedTemplateArgs) < 3 {
		t.Fatal("template args too short")
	}
	if capturedTemplateArgs[1] != "oberth" {
		t.Fatalf("release = %q, want oberth", capturedTemplateArgs[1])
	}
	if capturedTemplateArgs[2] != "charts/oberth" {
		t.Fatalf("chart = %q, want charts/oberth", capturedTemplateArgs[2])
	}
}

func TestPreflightDrift_ContextPassedToHelm(t *testing.T) {
	t.Parallel()
	var getArgs, templateArgs []string
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			getArgs = args
			return []byte("key: value\n"), nil
		}
		if args[0] == "template" {
			templateArgs = args
			return []byte("---\n"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	// The resolver receives the context name and returns --kube-context.
	resolver := func(ctx string) ([]string, string, error) {
		if ctx != "my-ctx" {
			t.Fatalf("resolver received context %q, want my-ctx", ctx)
		}
		return []string{"--kube-context", ctx}, "context " + ctx, nil
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), []string{"--context", "my-ctx"}, &out, helm, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertContainsFlag := func(label string, args []string, flag, value string) {
		t.Helper()
		for i, a := range args {
			if a == flag && i+1 < len(args) && args[i+1] == value {
				return
			}
		}
		t.Fatalf("%s args should contain %s %s: %v", label, flag, value, args)
	}
	assertContainsFlag("get", getArgs, "--kube-context", "my-ctx")
	assertContainsFlag("template", templateArgs, "--kube-context", "my-ctx")
}

func TestPreflightDrift_CustomFlags(t *testing.T) {
	t.Parallel()
	var capturedGetArgs, capturedTemplateArgs []string
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			capturedGetArgs = args
			return []byte("key: value\n"), nil
		}
		if args[0] == "template" {
			capturedTemplateArgs = args
			return []byte("---\n"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), []string{
		"--chart", "/path/to/chart",
		"--release", "my-release",
		"--namespace", "my-ns",
	}, &out, helm, fakeResolver("context default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify get values used custom release and namespace.
	found := false
	for i, a := range capturedGetArgs {
		if a == "my-release" {
			found = true
			_ = i
		}
	}
	if !found {
		t.Fatalf("get values should use release my-release: %v", capturedGetArgs)
	}
	// Verify template used custom chart path.
	if capturedTemplateArgs[2] != "/path/to/chart" {
		t.Fatalf("chart = %q, want /path/to/chart", capturedTemplateArgs[2])
	}
}

func TestPreflightDrift_NullValues(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return []byte("null\n"), nil
		}
		if args[0] == "template" {
			return []byte("---\n"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Fatalf("output should warn about null values: %s", out.String())
	}
}

// TestPreflightDrift_OKNamesTarget asserts that the OK line includes the
// resolved target so the operator can tell what was actually checked (#460).
func TestPreflightDrift_OKNamesTarget(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return []byte("key: value\n"), nil
		}
		if args[0] == "template" {
			return []byte("---\n"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context default (from /etc/rancher/k3s/k3s.yaml)"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "context default (from /etc/rancher/k3s/k3s.yaml)") {
		t.Fatalf("OK line should name the resolved target: %s", out.String())
	}
}

// TestPreflightDrift_K3sFallbackPassesKubeconfig asserts that when the resolver
// returns --kubeconfig (k3s fallback), both helm invocations receive it (#460).
func TestPreflightDrift_K3sFallbackPassesKubeconfig(t *testing.T) {
	t.Parallel()
	var getArgs, templateArgs []string
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			getArgs = args
			return []byte("key: value\n"), nil
		}
		if args[0] == "template" {
			templateArgs = args
			return []byte("---\n"), nil
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	k3sPath := "/etc/rancher/k3s/k3s.yaml"
	resolver := fakeResolver("context default (from "+k3sPath+")", "--kubeconfig", k3sPath)
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertContainsFlag := func(label string, args []string, flag, value string) {
		t.Helper()
		for i, a := range args {
			if a == flag && i+1 < len(args) && args[i+1] == value {
				return
			}
		}
		t.Fatalf("%s args should contain %s %s: %v", label, flag, value, args)
	}
	assertContainsFlag("get", getArgs, "--kubeconfig", k3sPath)
	assertContainsFlag("template", templateArgs, "--kubeconfig", k3sPath)
}

// TestPreflightDrift_ErrorNamesTarget asserts that error text names the
// resolved target so the operator can diagnose mis-targeting (#460).
func TestPreflightDrift_ErrorNamesTarget(t *testing.T) {
	t.Parallel()
	helm := func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "get" {
			return nil, fmt.Errorf("Error: release: not found")
		}
		return nil, fmt.Errorf("unexpected: %v", args)
	}
	var out bytes.Buffer
	err := runPreflightDrift(t.Context(), nil, &out, helm, fakeResolver("context gke_project_zone_cluster"))
	if err == nil {
		t.Fatal("expected error for missing release")
	}
	if !strings.Contains(err.Error(), "context gke_project_zone_cluster") {
		t.Fatalf("error should name the resolved target: %v", err)
	}
	if !strings.Contains(err.Error(), "--context") {
		t.Fatalf("error should suggest --context: %v", err)
	}
}
