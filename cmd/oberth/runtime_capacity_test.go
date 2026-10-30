package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestRuntimeCapacityServeFlagKeepsBootDefault(t *testing.T) {
	opts, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines", "--max-concurrent-jobs=3", "--max-concurrent-jobs-file=/etc/oberth/capacity"}, io.Discard)
	if err != nil || opts.maxConcurrent != 3 || opts.maxConcurrentFile != "/etc/oberth/capacity" {
		t.Fatal(opts, err)
	}
}
func TestRuntimeCapacityChartProjectedDirectoryAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sets    []string
		value   string
		enabled bool
	}{
		{name: "default", value: "3", enabled: true},
		{name: "boot-default", sets: []string{"maxConcurrentJobs=8"}, value: "8", enabled: true},
		{name: "override", sets: []string{"admission.runtimeCapacity.maxConcurrentJobs=5"}, value: "5", enabled: true},
		{name: "disabled", sets: []string{"admission.runtimeCapacity.enabled=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
			for _, set := range tc.sets {
				args = append(args, "--set", set)
			}
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v %s", err, output)
			}
			var deployment appsv1.Deployment
			var config corev1.ConfigMap
			for _, doc := range strings.Split(string(output), "\n---\n") {
				var kind struct{ Kind string }
				if err := yaml.Unmarshal([]byte(doc), &kind); err != nil {
					t.Fatal(err)
				}
				if kind.Kind == "Deployment" {
					if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
						t.Fatal(err)
					}
				}
				if kind.Kind == "ConfigMap" {
					var candidate corev1.ConfigMap
					if err := yaml.Unmarshal([]byte(doc), &candidate); err != nil {
						t.Fatal(err)
					}
					if strings.HasSuffix(candidate.Name, "-admission") {
						config = candidate
					}
				}
			}
			if len(deployment.Spec.Template.Spec.Containers) != 1 {
				t.Fatal("server container missing")
			}
			container := deployment.Spec.Template.Spec.Containers[0]
			mounted, flag, volume := false, false, false
			for _, arg := range container.Args {
				if strings.HasPrefix(arg, "--max-concurrent-jobs-file=") {
					flag = true
				}
			}
			for _, mount := range container.VolumeMounts {
				if mount.Name == "runtime-capacity" {
					mounted = true
					if !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" || mount.MountPath != "/etc/oberth/runtime-capacity" {
						t.Fatal("ConfigMap mount pinned or writable", mount)
					}
				}
			}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				if v.Name == "runtime-capacity" {
					volume = true
					if v.ConfigMap == nil || v.ConfigMap.Name != config.Name || v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
						t.Fatal("wrong ConfigMap reference", v)
					}
				}
			}
			if flag != tc.enabled || mounted != tc.enabled || volume != tc.enabled {
				t.Fatal("partial runtime capacity wiring", flag, mounted, volume)
			}
			if tc.enabled && config.Data["max-concurrent-jobs"] != tc.value {
				t.Fatal(config)
			}
			if !tc.enabled && config.Name != "" {
				t.Fatal("disabled ConfigMap rendered")
			}
			// Live edits must not change the pod template through a ConfigMap checksum.
			for name := range deployment.Spec.Template.Annotations {
				if strings.Contains(name, "runtime-capacity") || strings.Contains(name, "admission") {
					t.Fatal("runtime ConfigMap triggers rollout", name)
				}
			}
		})
	}
	for _, value := range []string{"0", "65", "-1", "wrong"} {
		output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set", "admission.runtimeCapacity.maxConcurrentJobs="+value).CombinedOutput()
		if err == nil {
			t.Fatal("invalid chart capacity accepted", value, string(output))
		}
	}
}
