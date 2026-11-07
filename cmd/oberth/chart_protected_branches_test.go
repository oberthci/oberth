package main

import (
	"os/exec"
	"strings"
	"testing"
)

// branches.protected: the default lists main,master; [] protects only each
// repository's default branch; null keeps the server default (#1490).
func TestChartProtectedBranchesExpressesEmptyAndDefault(t *testing.T) {
	for _, tc := range []struct {
		arguments []string
		want      string
	}{
		{nil, `"--protected-branches=main,master"`},
		{[]string{"--set-json", "branches.protected=[]"}, `"--protected-branches="`},
		{[]string{"--set-json", `branches.protected=["release/*"]`}, `"--protected-branches=release/*"`},
		{[]string{"--set", "branches.protected=null"}, ""},
	} {
		arguments := append([]string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest, "-s", "templates/deployment.yaml"}, tc.arguments...)
		output, err := exec.Command("helm", arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("render %v: %v\n%s", tc.arguments, err, output)
		}
		rendered := string(output)
		if tc.want == "" {
			if strings.Contains(rendered, "--protected-branches") {
				t.Fatalf("null rendered a flag: %v", tc.arguments)
			}
		} else if !strings.Contains(rendered, tc.want) {
			t.Fatalf("%v: missing %s", tc.arguments, tc.want)
		}
	}
}
