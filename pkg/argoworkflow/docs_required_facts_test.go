package argoworkflow

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestOperationsExplainsParallelMarks(t *testing.T) {
	raw, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, fact := range []string{"web rail", "CLI/agent `┌│└`", "parallel groups with visible ends"} {
		if !strings.Contains(text, fact) {
			t.Fatal("operations omits parallel display guidance", fact)
		}
	}
}

func TestOperationsPointsMissingTagsToPublication(t *testing.T) {
	raw, err := os.ReadFile("../../docs/operations.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(text, "check publication if a tag has not reached the forge") {
		t.Fatal("operations must direct missing tags to publication")
	}
}

func TestDocumentationStorageAndPlanContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), ",", "")), " ")
	for _, fact := range []string{
		"The single run cap is Argo `parallelism`",
		"`volumeClaimTemplates`",
		"The admitted plan is immutable",
		"Pending entries exist before Pods start",
		"retries stay attempts of one logical step",
		"Dynamic placeholders represent unresolved expansion",
		"`step_count` is a lower bound while `steps_planned_exact` is false",
		"Observations expand the display, not the plan",
		fmt.Sprintf("Pipeline files have a %d MiB limit", admissioncontract.MaxDocumentBytes>>20),
		fmt.Sprintf("submitted source-consuming Workflow specs have a %d KiB limit", admissioncontract.MaxSourceSpecBytes>>10),
		fmt.Sprintf("Display plan limits: %d entries / %d MiB", admissioncontract.MaxPlanEntries, admissioncontract.MaxPlanBytes>>20),
		"excess expansion collapses to dynamic placeholders",
		"These plan budgets do not limit native fan-out or refuse admission for expansion",
		"server reserves at least 1856 MiB of source-delivery scratch, separate from step storage",
	} {
		if !strings.Contains(text, strings.ReplaceAll(fact, ",", "")) {
			t.Errorf("pipeline guide omits required fact: %s", fact)
		}
	}
}

// Resource checks cover only the manifest envelope, so the guide must not
// promise that a Pod-creating capability stays off the node.
func TestDocumentationStatesThePodCapabilityLimit(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(text, "A capability over a Pod-creating kind (Pod, Job, virtual machine) runs any Pod in that namespace, bounded only by its Pod Security and the account's RBAC") || strings.Contains(text, "grants no node access") {
		t.Fatal("pipeline guide misstates what a Pod-creating capability allows")
	}
}

func TestDocumentationReleaseAccountNameRule(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/releasing.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[release account-name rule](../internal/identityname/name.go)") {
		t.Fatal("release guide must link the account-name authority")
	}
}

func TestDocumentationStatesSynchronizationScope(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(text, "Use resource requests and affinity for capacity; use repository mutexes for exclusive access.") {
		t.Fatal("pipeline guide omits native capacity and exclusivity controls")
	}
	if !strings.Contains(text, "Use literal `upstream/org/repo/lock` for your repository, e.g. `forge/team/app/publish`.") {
		t.Fatal("pipeline guide omits the mutex form beside its example")
	}
}

func TestDocumentationUsesNativeCapacityAndPublicationLocks(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pipelines", "releasing"} {
		raw, err := os.ReadFile("../../docs/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "oberth-sync-limits") {
			t.Fatal("guide refers to shared lock sizes", name)
		}
		if name == "releasing" && !strings.Contains(string(raw), "upstream/org/repo/release-publish") {
			t.Fatal("release guide omits repository publication mutex")
		}
	}
}
