package main

import (
	"os/exec"
	"strings"
	"testing"
)

// branches.reserved: the default lists main,master; [] protects only each
// repository's default branch; null keeps the server default (#1490).
func TestChartReservedBranchesExpressesEmptyAndDefault(t *testing.T) {
	for _, tc := range []struct {
		arguments []string
		want      string
	}{
		{nil, `"--reserved-branches=main,master"`},
		{[]string{"--set-json", "branches.reserved=[]"}, `"--reserved-branches="`},
		{[]string{"--set-json", `branches.reserved=["release/*"]`}, `"--reserved-branches=release/*"`},
		{[]string{"--set", "branches.reserved=null"}, ""},
	} {
		arguments := append([]string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest, "-s", "templates/deployment.yaml"}, tc.arguments...)
		output, err := exec.Command("helm", arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("render %v: %v\n%s", tc.arguments, err, output)
		}
		rendered := string(output)
		if tc.want == "" {
			if strings.Contains(rendered, "--reserved-branches") {
				t.Fatalf("null rendered a flag: %v", tc.arguments)
			}
		} else if !strings.Contains(rendered, tc.want) {
			t.Fatalf("%v: missing %s", tc.arguments, tc.want)
		}
	}
}
