package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestKubernetesClientLimits(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		qps   float32
		burst int
	}{{nil, 50, 100}, {[]string{"--kubernetes-client-qps=75.5", "--kubernetes-client-burst=150"}, 75.5, 150}} {
		options, err := parseServeOptions(append([]string{"--argo-namespace=argo"}, tc.args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		original := &rest.Config{Host: "https://fixture.invalid"}
		configured := serverKubernetesConfig(original, options)
		if configured.QPS != tc.qps || configured.Burst != tc.burst || configured.Timeout != 10*time.Second || configured.Host != original.Host || original.QPS != 0 {
			t.Fatal("server Kubernetes client limits were not applied independently")
		}
	}
	for _, arg := range []string{"--kubernetes-client-qps=0", "--kubernetes-client-qps=-1", "--kubernetes-client-qps=NaN", "--kubernetes-client-qps=Inf", "--kubernetes-client-qps=1e100", "--kubernetes-client-burst=0", "--kubernetes-client-burst=-1"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", arg}, io.Discard); err == nil {
			t.Fatalf("accepted %s", arg)
		}
	}
}

func TestChartKubernetesClientLimits(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		qps, burst string
	}{
		{nil, "50", "100"},
		{[]string{"--set", "kubernetesClient=null"}, "50", "100"},
		{[]string{"--set-json", `kubernetesClient={"qps":75.5,"burst":150}`}, "75.5", "150"},
	} {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest, "-s", "templates/deployment.yaml"}
		out, err := exec.Command("helm", append(args, tc.args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("render: %v: %s", err, out)
		}
		for _, flag := range []string{"--kubernetes-client-qps=" + tc.qps, "--kubernetes-client-burst=" + tc.burst} {
			if !strings.Contains(string(out), flag) {
				t.Fatalf("missing rendered %s", flag)
			}
		}
	}
}
