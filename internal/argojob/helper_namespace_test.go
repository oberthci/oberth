package argojob

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func namespaceUnavailable(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("OBERTH_REQUIRE_HOLD_NAMESPACES") == "1" {
		t.Fatalf("required hold namespace test cannot run: %v", err)
	}
	t.Skipf("hold-namespaces: SKIPPED: private user, mount and PID namespaces unavailable: %v", err)
}

func TestNamespaceSkipProcess(t *testing.T) {
	if os.Getenv("OBERTH_HOLD_TEST_NAMESPACE_UNAVAILABLE") == "1" {
		namespaceUnavailable(t, errors.New("fixture: user namespaces disabled"))
	}
}

func TestNamespaceSkipPolicy(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBERTH_HOLD_TEST_NAMESPACE_UNAVAILABLE", "1")
	for _, required := range []string{"0", "1"} {
		t.Setenv("OBERTH_REQUIRE_HOLD_NAMESPACES", required)
		cmd := exec.CommandContext(t.Context(), self, "-test.v", "-test.run=^TestNamespaceSkipProcess$")
		out, err := cmd.CombinedOutput()
		if required == "0" {
			if err != nil || !strings.Contains(string(out), "--- SKIP: TestNamespaceSkipProcess") || !strings.Contains(string(out), "fixture: user namespaces disabled") {
				t.Fatal("namespace skip did not report its reason", string(out), err)
			}
		} else if err == nil || !strings.Contains(string(out), "required hold namespace test cannot run") || !strings.Contains(string(out), "--- FAIL: TestNamespaceSkipProcess") {
			t.Fatal("required namespace test silently skipped", string(out), err)
		}
	}
}
