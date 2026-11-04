package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestChartDisabledPipelinePolicyPersistsInStatus(t *testing.T) {
	output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--show-only", "templates/deployment.yaml", "--set", "image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64), "--set", "networkPolicy.enabled=false").CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v\n%s", err, output)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(output, &deployment); err != nil {
		t.Fatal(err)
	}
	args := deployment.Spec.Template.Spec.Containers[0].Args
	options, err := parseServeOptions(args[1:], io.Discard)
	if err != nil {
		t.Fatalf("parse chart args %v: %v", args, err)
	}
	health := configuredHealth(options)
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disabled deployment status: %s", payload)
	if !strings.Contains(string(payload), "WARNING: pipeline NetworkPolicy is configured disabled") {
		t.Fatalf("configured-disabled policy absent from status: %s", payload)
	}
	configure(t, remoteServer(t, string(payload)))
	var out bytes.Buffer
	if err := runRemoteStatus(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING: pipeline NetworkPolicy is configured disabled") {
		t.Fatalf("disabled policy absent from CLI: %s", out.String())
	}
}

func TestChartEnabledPipelinePolicyStatus(t *testing.T) {
	for _, setting := range []string{"", "true"} {
		t.Run("enabled="+setting, func(t *testing.T) {
			arguments := []string{"template", "oberth", "../../charts/oberth", "--show-only", "templates/deployment.yaml", "--set", "image.ref=example.invalid/oberth@sha256:" + strings.Repeat("1", 64)}
			if setting != "" {
				arguments = append(arguments, "--set", "networkPolicy.enabled="+setting)
			}
			output, err := exec.Command("helm", arguments...).CombinedOutput()
			if err != nil {
				t.Fatalf("render chart: %v\n%s", err, output)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(output, &deployment); err != nil {
				t.Fatal(err)
			}
			options, err := parseServeOptions(deployment.Spec.Template.Spec.Containers[0].Args[1:], io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			health := configuredHealth(options)
			value, err := health.Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			want := "pipeline NetworkPolicy is configured enabled; CNI enforcement has not been measured"
			if !strings.Contains(string(payload), want) || strings.Contains(string(payload), "WARNING") {
				t.Fatalf("enabled config misreported: %s", payload)
			}
			configure(t, remoteServer(t, string(payload)))
			var out bytes.Buffer
			if err := runRemoteStatus(t.Context(), nil, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), want) {
				t.Fatalf("CLI hides config: %s", out.String())
			}
		})
	}
}

func TestPipelinePolicyConfigurationUnavailableWithoutFlag(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	health := configuredHealth(options)
	value, err := health.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "pipeline NetworkPolicy configuration is unavailable; CNI enforcement has not been measured") {
		t.Fatalf("missing configuration guessed: %s", payload)
	}
	configure(t, remoteServer(t, string(payload)))
	var out bytes.Buffer
	if err := runRemoteStatus(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "configuration is unavailable") {
		t.Fatalf("CLI hides unavailable config: %s", out.String())
	}
}

func TestPipelinePolicyConfigurationRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "auto", "unknown", "yes", "TRUE"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--pipeline-network-policy-enabled=" + value}, io.Discard); err == nil || !strings.Contains(err.Error(), "pipeline NetworkPolicy configuration must be true or false") {
			t.Fatalf("value=%q err=%v", value, err)
		}
	}
}
