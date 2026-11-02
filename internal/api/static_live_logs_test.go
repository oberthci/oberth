package api

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

func TestDashboardBehaviorRequiresNode(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-test.run=^TestDashboardLiveLogBehavior$", "-test.v")
	command.Env = append(os.Environ(), "PATH="+t.TempDir())
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "dashboard behavior checks require Node on PATH") {
		t.Fatalf("missing Node must fail the test: %v\n%s", err, output)
	}
}

// CI test leaves provision pinned Node with hack/with-test-node.sh. Keep a
// missing runtime fatal so dashboard coverage cannot silently disappear.
func TestDashboardLiveLogBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("dashboard behavior checks require Node on PATH: install Node or use hack/with-test-node.sh on Linux/x86_64")
	}
	for _, script := range []string{"test-live-logs.mjs", "test-dashboard-runs.mjs", "test-resource-messages.mjs", "test-dashboard-token-expiry.mjs"} {
		t.Run(script, func(t *testing.T) {
			command := exec.Command(node, "../../hack/"+script)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("dashboard behavior: %v\n%s", err, output)
			}
		})
	}
}

func TestDashboardDeliveryWirePrefix(t *testing.T) {
	source := string(staticAssets["app.js"].body)
	if !strings.Contains(source, "const deliveryPrefix = "+strconv.Quote(runprogress.LogDeliveryPrefix)+";") {
		t.Fatal("dashboard and retained log delivery prefixes differ")
	}
}
