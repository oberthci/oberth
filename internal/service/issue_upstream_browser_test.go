package service_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestIssueUpstreamViewerBrowser(t *testing.T) {
	chromium, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("browser assertion requires chromium")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("browser assertion requires node")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), node, "../../hack/webui/issue-upstreams.mjs")
	command.Env = append(os.Environ(), "OBERTH_UI_BINARY="+binary, "OBERTH_UI_ARTIFACTS="+t.TempDir(), "CHROMIUM_BIN="+chromium)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("issue upstream browser: %v\n%s", err, output)
	}
	t.Log(string(output))
}
