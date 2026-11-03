package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestWorkflowCapabilityToolsEnforceAdminScopeAndRevocation(t *testing.T) {
	f := newScopeFixture(t)
	control, err := NewAPI(APIConfig{Runs: f.store, Repositories: f.store, WorkflowCapabilities: f.store})
	if err != nil {
		t.Fatal(err)
	}
	args := func(repo string) json.RawMessage {
		b, e := json.Marshal(map[string]any{"repo": repo, "capability": map[string]any{"namespace": "pipelines", "service_account": "operator-worker", "extended_resources": map[string]int{"devices.example.test/worker": 2}}})
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	ctx := t.Context()
	if _, err = control.CallTool(ctx, scopedActor, "workflow_capability_grant", args(f.visible.Name)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin grant: %v", err)
	}
	if _, err = control.CallTool(ctx, scopedAdmin, "workflow_capability_grant", args(f.hidden.Name)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-scope admin grant: %v", err)
	}
	if cap, e := f.store.ActiveWorkflowCapability(ctx, f.hidden.ID); e != nil || cap != nil {
		t.Fatalf("hidden repository changed: %+v %v", cap, e)
	}
	for _, tool := range []string{"workflow_capability_get", "workflow_capability_grant", "workflow_capability_revoke"} {
		for _, repo := range []string{f.hidden.Name, "codeberg/exampleorg/does-not-exist"} {
			arguments, marshalErr := json.Marshal(map[string]string{"repo": repo})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if tool == "workflow_capability_grant" {
				arguments = args(repo)
			}
			if _, err = control.CallTool(ctx, scopedAdmin, tool, arguments); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%s disclosed repository %q: %v", tool, repo, err)
			}
		}
	}
	if _, err = control.CallTool(ctx, scopedAdmin, "workflow_capability_grant", args(f.visible.Name)); err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(ctx, scopedActor, "workflow_capability_get", json.RawMessage(`{"repo":"oberth"}`))
	if err != nil || value.(map[string]any)["granted"] != true {
		t.Fatalf("in-scope capability read: %v, %v", value, err)
	}
	raw := json.RawMessage(`{"repo":"oberth"}`)
	if _, err = control.CallTool(ctx, scopedActor, "workflow_capability_revoke", raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin revoke: %v", err)
	}
	if _, err = control.CallTool(ctx, scopedAdmin, "workflow_capability_revoke", raw); err != nil {
		t.Fatal(err)
	}
	if cap, e := f.store.ActiveWorkflowCapability(ctx, f.visible.ID); e != nil || cap != nil {
		t.Fatalf("revoked grant active: %+v %v", cap, e)
	}
	if _, err = control.CallTool(ctx, scopedAdmin, "workflow_capability_grant", raw); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing capability silently revoked: %v", err)
	}
	if !MutatingTool("workflow_capability_grant") || !MutatingTool("workflow_capability_revoke") || MutatingTool("workflow_capability_get") {
		t.Fatal("capability mutation gate mismatch")
	}
}
