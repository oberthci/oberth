package apicontract_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestToolDescriptionsNameOnlyAvailableTools(t *testing.T) {
	definitions := apicontract.Definitions(apicontract.Bindings{})
	names := map[string]bool{}
	for _, d := range definitions {
		names[d.Name] = true
	}
	// These are continuation fields, not operation references.
	fields := map[string]bool{"after_id": true, "next_after_id": true, "next_byte_offset": true, "next_step_offset": true}
	references := regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
	for _, d := range definitions {
		for _, name := range references.FindAllString(d.Description, -1) {
			if !names[name] && !fields[name] {
				t.Errorf("%s names unavailable tool %s", d.Name, name)
			}
		}
	}
}
func TestGuidanceDescribesOperationAndSelectorMeaning(t *testing.T) {
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Name == "sync" && (!strings.Contains(d.Description, "parked") || !strings.Contains(d.Description, "upstream without running")) {
			t.Fatal("sync publication guidance lost", d.Description)
		}
		if d.Name == "events_wait" && (!strings.Contains(d.Description, "after a gap") || !strings.Contains(d.Description, "after_id")) {
			t.Fatal("event cursor recovery guidance lost", d.Description)
		}
	}
}
func TestWaitTriggerGuidanceListsFilters(t *testing.T) {
	field, _ := reflect.TypeFor[apicontract.WaitArgs]().FieldByName("Trigger")
	for _, text := range []string{"Filter", "branch", "promotion", "schedule", "tag"} {
		if !strings.Contains(field.Tag.Get("desc"), text) {
			t.Fatal("wait trigger is not a documented filter")
		}
	}
}
func TestIssueIDGuidanceUsesIntegerIDs(t *testing.T) {
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if !strings.HasPrefix(d.Name, "issue_") {
			continue
		}
		field, ok := d.InputType.FieldByName("ID")
		if ok && (!strings.Contains(field.Tag.Get("desc"), "integer issue ID") || strings.Contains(field.Tag.Get("desc"), "prefix")) {
			t.Fatal("issue ID has run guidance", d.Name)
		}
	}
}
func TestDeclaredMutationHasExactlyOneReadTool(t *testing.T) {
	mutating := strings.Fields(`
 access_allow access_dismiss access_revoke admin_audit_record client_renew
 issue_import issue_append issue_close issue_create issue_delete issue_lock issue_reopen issue_update
 promote promote_retry publication_abandon publish_retry receive_outbox
 repo_add repo_archive repo_remove repo_unarchive
 run_cancel run_hold_abandon run_hold_exec run_purge run_rerun
 schedule_pause schedule_resume schedule_trigger sync secret_put
 uplink_add uplink_cert uplink_invite uplink_revoke uplink_rotate uplink_scope
 upstream_add upstream_provide_key upstream_remove
 workflow_capability_grant workflow_capability_revoke
 `)
	want := make(map[string]bool, len(mutating))
	for _, name := range mutating {
		want[name] = true
	}
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Mutation != want[d.Name] || (d.Check != "") != want[d.Name] {
			t.Errorf("%s: mutation flag or read tool disagrees with the fixed action inventory", d.Name)
		}
		delete(want, d.Name)
	}
	if len(want) != 0 {
		t.Fatal("mutating tools are missing", want)
	}
}
