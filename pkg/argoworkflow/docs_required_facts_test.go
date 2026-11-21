package argoworkflow

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestDocumentationStorageAndPlanContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), ",", "")), " ")
	for _, fact := range []string{
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
