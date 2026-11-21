package app_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestWorkflowNameHasOneFixedForm(t *testing.T) {
	t.Parallel()
	const want = "run-82f3e9c695dc6b8d1b11818d5701919e"
	if got := nativebinding.WorkflowName("r1"); got != want {
		t.Fatal(got)
	}
	for _, id := range []string{"", "r1", strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 4096), "Run:/with spaces/雪"} {
		name := nativebinding.WorkflowName(id)
		if len(name) != 36 || !strings.HasPrefix(name, "run-") {
			t.Fatal("workflow name has variable form", name)
		}
		for _, char := range strings.TrimPrefix(name, "run-") {
			if !strings.ContainsRune("0123456789abcdef", char) {
				t.Fatal("workflow name is not a DNS label", name)
			}
		}
	}
}
func TestWorkflowNamesArePrefixFreeForDistinctRunIDs(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, id := range []string{"r1", "r1-x", strings.Repeat("a", 63) + "1", strings.Repeat("a", 63) + "2"} {
		name := nativebinding.WorkflowName(id)
		if names[name] {
			t.Fatal("run IDs share a workflow name", id, name)
		}
		for old := range names {
			if strings.HasPrefix(name, old+"-") || strings.HasPrefix(old, name+"-") {
				t.Fatal("workflow names can extend each other", old, name)
			}
		}
		names[name] = true
	}
}
