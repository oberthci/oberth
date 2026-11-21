package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	"github.com/oberthci/oberth/internal/stateport"
)

type revokeStore struct {
	backend     *storefake.Store
	writer      secretstore.Writer
	before      func()
	beforeWrite func()
	unreachable bool
	refused     bool
	absent      bool
}

func attachRevokeStore(t *testing.T, p *peopleFixture) *revokeStore {
	t.Helper()
	f := &revokeStore{backend: &storefake.Store{}}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	cfg := secretstore.Config{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "oberth-writer", StepRole: "oberth-step", ServerNamespace: "oberth", PipelineNamespace: "ci", ServerAccount: "oberth", Audience: "store"}
	f.writer = secretstore.Writer{Client: client, Config: cfg, Token: func(context.Context) ([]byte, error) { return []byte("server-projection"), nil }}
	p.f.c.People.SecretWriter = f.writer
	p.f.restart(t)
	return f
}

func (f *revokeStore) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == "POST" && strings.Contains(r.URL.Path, "/steps/") && f.beforeWrite != nil {
		f.beforeWrite()
	}
	if f.unreachable {
		return nil, errors.New("private-store-response")
	}
	if r.Method == "DELETE" && f.before != nil {
		f.before()
	}
	response, err := f.backend.RoundTrip(r)
	if err == nil {
		f.adjustResponse(r, response)
	}
	return response, err
}

func (f *revokeStore) adjustResponse(r *http.Request, response *http.Response) {
	if f.refused && strings.HasSuffix(r.URL.Path, "/login") {
		_ = response.Body.Close()
		response.StatusCode = http.StatusForbidden
		response.Body = io.NopCloser(strings.NewReader("private-store-response"))
	}
	if r.Method == "DELETE" && f.absent {
		_ = response.Body.Close()
		response.StatusCode = http.StatusNotFound
		response.Body = io.NopCloser(strings.NewReader("private-store-response"))
	}
}

func revokeSecret(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs) (apicontract.Receipt, error) {
	t.Helper()
	call, err := p.f.s.PrepareAccessRevoke(t.Context(), adminCaller(), apicontract.AccessRevokeArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	return execute(t.Context(), call, err)
}

func secretAccountFor(t *testing.T, q apicontract.SecretPutArgs, kind identityname.Kind) string {
	t.Helper()
	account, err := identityname.Name(identityname.Key{Repository: q.Repo, File: q.Workflow, Step: q.Step}, kind)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func storedPath(account, path string) string { return "/oberth/steps/" + account + "/" + path }

func seedSecret(t *testing.T, f *revokeStore, account, path string) {
	t.Helper()
	if err := f.writer.Put(t.Context(), account, path, []byte("private-delete-value")); err != nil {
		t.Fatal(err)
	}
}

func deleteCalls(f *revokeStore) int {
	count := 0
	for _, call := range f.backend.Calls {
		if call.Method == "DELETE" {
			count++
		}
	}
	return count
}

func assertGrantState(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs, revoked bool) {
	t.Helper()
	snapshot, err := p.state.Grants(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range snapshot.Grants {
		if g.Kind == stateport.GrantStorePath && g.Repository == q.Repo && g.File == q.Workflow && g.Step == q.Step && g.Path == q.Secret {
			if g.Revoke != revoked {
				t.Fatal("grant revocation differs", g.Revoke)
			}
			return
		}
	}
	t.Fatal("exact grant missing")
}

func deleteAudit(t *testing.T, p *peopleFixture) []stateport.AuditRecord {
	t.Helper()
	rows, err := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "secret.delete.requested", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertDeletePublic(t *testing.T, p *peopleFixture, receipt apicontract.Receipt, err error) {
	t.Helper()
	rows, e := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 100})
	if e != nil {
		t.Fatal(e)
	}
	events, e := p.state.Events(t.Context(), stateport.EventFilter{Repositories: []string{repository}, Limit: 100})
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal([]any{rows, events, receipt})
	if e != nil {
		t.Fatal(e)
	}
	observed := string(raw)
	if err != nil {
		observed += err.Error()
	}
	for _, value := range []string{"private-delete-value", "private-store-response", "private-audit-failure"} {
		assertSecretAbsent(t, []byte(observed), []byte(value))
		digest := sha256.Sum256([]byte(value))
		if strings.Contains(observed, hex.EncodeToString(digest[:])) {
			t.Fatal("private digest entered an observation")
		}
	}
}
