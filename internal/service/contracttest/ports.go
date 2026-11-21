// Package contracttest specifies the service-owned authority and effect ports.
//
// Spec: docs/plans/c2-service-actions-and-history.md#allowed-dependencies
package contracttest

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

// AuthorityStore checks live identity, detached scope, explicit key policy and
// rejection of an unregistered credential. The supplied actor must be scoped.
func AuthorityStore(t *testing.T, port service.AuthorityStore, a actor.Actor) {
	t.Helper()
	ctx := context.Background()
	current, err := port.Current(ctx, a)
	if err != nil || current.Identity != a.Identity {
		t.Fatalf("current: %+v %v", current, err)
	}
	if len(current.Scope) == 0 {
		t.Fatal("contract fixture needs scoped actor")
	}
	current.Scope[0] = "changed"
	again, err := port.Current(ctx, a)
	if err != nil || !reflect.DeepEqual(again.Scope, a.Scope) {
		t.Fatal("scope aliases stored authority")
	}
	if _, err := port.KeyPolicy(ctx, a); err != nil {
		t.Fatal(err)
	}
	unknown := a
	unknown.Fingerprint = "unknown"
	if _, err := port.Current(ctx, unknown); err == nil {
		t.Fatal("unknown credential authenticated")
	}
	if _, err := port.KeyPolicy(ctx, unknown); err == nil {
		t.Fatal("unknown credential policy guessed")
	}
}

func Catalog(t *testing.T, port service.CatalogStore, a actor.Actor, repo string) {
	t.Helper()
	scope, err := port.RepositoryScope(t.Context(), a.Scope)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := port.Repositories(t.Context(), stateport.CatalogFilter{Repositories: scope, Limit: 200, IncludeArchived: true})
	if err != nil || len(rows) != 1 || rows[0].Canonical != repo {
		t.Fatalf("catalog %+v %v", rows, err)
	}
	r, err := port.Repository(t.Context(), repo, scope)
	if err != nil || r.Canonical != repo {
		t.Fatalf("repository %+v %v", r, err)
	}
	if _, err := port.Repository(t.Context(), repo, []string{}); err == nil {
		t.Fatal("empty scope resolved repository")
	}
}
func Effects(t *testing.T, exec service.Exec, logs service.Logs, r stateport.Run) {
	t.Helper()
	ctx := t.Context()
	if err := exec.Cancel(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	checked, err := exec.Inspect(ctx, service.CheckRequest{Request: model.CIRequest{Repo: r.Repo, Ref: r.Ref}})
	if err != nil || checked.Repo != r.Repo {
		t.Fatal("inspect", checked, err)
	}
	if _, err := exec.Locks(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := logs.Read(ctx, r, apicontract.RunLogsArgs{ID: r.ID})
	if err != nil || got.Content == "" {
		t.Fatal("logs", got, err)
	}
	for range 2 {
		if err := logs.Purge(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}
func Starter(t *testing.T, port service.Starter, q service.StartRequest) {
	t.Helper()
	out, err := port.StartRun(t.Context(), q)
	if err != nil || out.ID == "" {
		t.Fatal("start", out, err)
	}
}
