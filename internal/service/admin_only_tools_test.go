package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

// adminOnlyArguments are the arguments an admin-only tool needs before its
// admin check; every other admin-only tool refuses with empty arguments.
var adminOnlyArguments = map[string]map[string]any{
	"workflow_capability_grant":  {"repo": "oberth", "capability": map[string]any{"namespace": "pipelines", "service_account": "operator-worker"}},
	"workflow_capability_revoke": {"repo": "oberth"},
}

// tools/list hides api.AdminOnlyTool tools from non-admin tokens. This proves the
// list against the service: each one refuses a non-admin uplink as
// forbidden, and no other tool refuses a non-admin's empty call as
// forbidden, so a tool that becomes admin-only must join the list.
func TestAdminOnlyToolsRefuseNonAdmins(t *testing.T) {
	fixture := newScopeFixture(t)
	call := func(name string, arguments map[string]any) error {
		if arguments == nil {
			arguments = map[string]any{}
		}
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err = fixture.service.CallTool(ctx, api.Actor{Identity: "agent@example-node", TokenCredentialID: "fixture-credential", AuthMethod: "cert"}, name, raw)
		return err
	}
	for _, name := range api.ToolNames() {
		err := call(name, adminOnlyArguments[name])
		forbidden := errors.Is(err, ErrForbidden)
		switch {
		case api.AdminOnlyTool(name) && !forbidden:
			t.Errorf("%s is listed admin-only but a non-admin call returned %v", name, err)
		case !api.AdminOnlyTool(name) && forbidden:
			t.Errorf("%s refuses a non-admin (%v): add it to api adminOnlyTools", name, err)
		}
	}
}
