package api

import (
	"os/exec"
	"strconv"
	"strings"

	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

// The release tooling provisions pinned Node; a developer may also execute
// `node hack/test-live-logs.mjs` directly. The Go-only runner has no JS runtime.
func TestDashboardLiveLogBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("dashboard behavior checks need Node: node hack/test-live-logs.mjs")
	}
	command := exec.Command(node, "../../hack/test-live-logs.mjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dashboard behavior: %v\n%s", err, output)
	}
}

func TestDashboardDeliveryWirePrefix(t *testing.T) {
	source := string(staticAssets["app.js"].body)
	if !strings.Contains(source, "const deliveryPrefix = "+strconv.Quote(runprogress.LogDeliveryPrefix)+";") {
		t.Fatal("dashboard and retained log delivery prefixes differ")
	}
}
