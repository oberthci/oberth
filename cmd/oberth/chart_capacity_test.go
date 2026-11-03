package main

import (
	"os/exec"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func TestChartAdmissionCapacity(t *testing.T) {
	base := []string{"template", "oberth", "../../charts/oberth", "--namespace", "ci", "--set", "image.ref=example.invalid/oberth@" + testImageDigest}
	for _, tc := range []struct{ value, want string }{{"", "1"}, {"1.5", "1.5"}, {"null", "1"}} {
		args := append([]string{}, base...)
		if tc.value != "" {
			args = append(args, "--set-json", "admission.overcommitFactor="+tc.value)
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--admission-overcommit-factor="+tc.want+"\n") {
			t.Fatalf("factor %q render: %v %s", tc.value, err, out)
		}
		found := false
		for _, body := range strings.Split(string(out), "\n---\n") {
			var doc struct {
				Kind     string
				Metadata struct{ Name string }
				Rules    []struct {
					APIGroups []string
					Resources []string
					Verbs     []string
				}
			}
			if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Kind != "ClusterRole" || !strings.HasSuffix(doc.Metadata.Name, "-allocatable") {
				continue
			}
			found = true
			if len(doc.Rules) != 1 || strings.Join(doc.Rules[0].APIGroups, ",") != "" || strings.Join(doc.Rules[0].Resources, ",") != "nodes" || strings.Join(doc.Rules[0].Verbs, ",") != "list" {
				t.Fatalf("capacity RBAC exceeds node list: %+v", doc)
			}
		}
		if !found {
			t.Fatal("missing node capacity RBAC")
		}
	}
	for _, bad := range []string{"0", "-1", "0.9", "NaN", "true"} {
		args := append(append([]string{}, base...), "--set", "admission.overcommitFactor="+bad)
		if err := exec.Command("helm", args...).Run(); err == nil {
			t.Fatalf("invalid factor %q accepted", bad)
		}
	}
}
