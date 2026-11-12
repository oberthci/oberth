package main

import (
	"os/exec"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestChartServerCPUReservationAndOverride(t *testing.T) {
	for _, test := range []struct {
		name, request, limit string
		sets                 []string
	}{
		{name: "default", request: "500m"},
		{name: "explicit limit", request: "750m", limit: "2", sets: []string{"resources.requests.cpu=750m", "resources.limits.cpu=2000m"}},
		{name: "clear saved limit", request: "500m", sets: []string{"resources.limits.cpu=null"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"template", "oberth", "../../charts/oberth", "--namespace", "ci", "--set", "image.ref=example.invalid/oberth@" + testImageDigest, "-s", "templates/deployment.yaml"}
			for _, set := range test.sets {
				args = append(args, "--set", set)
			}
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(output, &deployment); err != nil {
				t.Fatal(err)
			}
			if deployment.Spec.Template.Spec.PriorityClassName != "ci-oberth-control-plane" {
				t.Fatalf("lost control-plane priority: %s", deployment.Spec.Template.Spec.PriorityClassName)
			}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name != "oberth" {
					continue
				}
				if got := container.Resources.Requests.Cpu().String(); got != test.request {
					t.Fatalf("CPU request %s, want %s", got, test.request)
				}
				limit, exists := container.Resources.Limits[corev1.ResourceCPU]
				if test.limit == "" && exists {
					t.Fatalf("unexpected CPU limit: %s", limit.String())
				}
				if test.limit != "" && (!exists || limit.String() != test.limit) {
					t.Fatalf("CPU limit %s, want %s", limit.String(), test.limit)
				}
				if container.Resources.Limits.Memory().String() != "1Gi" || container.Resources.Limits.StorageEphemeral().String() != "2Gi" {
					t.Fatalf("lost memory/storage bounds: %+v", container.Resources.Limits)
				}
				return
			}
			t.Fatal("missing server container")
		})
	}
}
