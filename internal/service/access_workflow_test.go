package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestAccessToolsRequireAndPreserveWorkflowScope(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	s.secretAccess = f.store
	actor := api.Actor{Identity: "admin", Admin: true}
	const repo = "upstream/org/repo"
	call := func(name, file string) (any, error) {
		args := map[string]string{"repo": repo, "step": "publish", "secret": "oberth/data/release/token"}
		if file != "" {
			args["workflow"] = file
		}
		raw, _ := json.Marshal(args)
		return s.CallTool(t.Context(), actor, name, raw)
	}
	for _, name := range []string{"access_allow", "access_revoke"} {
		if _, err := call(name, ""); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s accepted absent workflow: %v", name, err)
		}
	}
	for _, file := range []string{".oberth/a.yaml", ".oberth/b.yaml"} {
		value, err := call("access_allow", file)
		if err != nil || value.(api.AccessGrantResponse).Workflow != file {
			t.Fatal(value, err)
		}
	}
	if _, err := call("access_revoke", ".oberth/a.yaml"); err != nil {
		t.Fatal(err)
	}
	listed, err := s.CallTool(t.Context(), actor, "access_list", json.RawMessage(`{"repo":"upstream/org/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	grants := listed.(api.AccessListResponse).Grants
	if len(grants) != 1 || grants[0].Workflow != ".oberth/b.yaml" {
		t.Fatal("revoke crossed files", grants)
	}
}
