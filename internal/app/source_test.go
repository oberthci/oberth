package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Covers: REQ-C3-001
func TestAtomicDeclarationRefreshContract(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "kv/build"}
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	addLogin(w)
	f.setDocument(w)
	f.d.Routes = workflowpolicy.ReleaseRoutes{request().Repository: {{Pattern: "v2.*", File: ".oberth/publish.yaml"}}}
	files := []string{}
	scopes := []app.Scope{}
	read := f.source.Read
	load := f.authority.LoadFunc
	f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
		if r.Commit != strings.Repeat("d", 40) {
			t.Errorf("not default head: %+v", r)
		}
		files = append(files, path)
		return read(ctx, r, path)
	}
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		scopes = append(scopes, scope)
		return load(ctx, scope)
	}
	s := f.new(t)
	pending, err := s.PendingGrants(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) < 3 || len(files) != 3 || len(scopes) != 3 {
		t.Fatal(pending, files, scopes)
	}
	for _, scope := range scopes {
		if scope.Release != (scope.File != workflowpolicy.BuildFile) {
			t.Fatal("wrong pending tier", scope)
		}
	}
	if f.creates.Load() != 0 {
		t.Fatal("pending granted execution")
	}
}
func TestPendingGrantsNeverReturnsPartialFindings(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"read", "invalid", "missing"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			read := f.source.Read
			f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
				if path != workflowpolicy.ReleaseFile {
					return read(ctx, r, path)
				}
				switch stage {
				case "read":
					return gitport.File{}, errors.New("read unavailable")
				case "missing":
					return missingFile(ctx, r, path)
				default:
					return gitport.File{Blob: strings.Repeat("c", 40), Bytes: []byte("[")}, nil
				}
			}
			pending, err := f.new(t).PendingGrants(context.Background(), request())
			if stage == "missing" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || pending != nil {
				t.Fatal("partial pending response", pending, err)
			}
		})
	}
}

type pinCase struct {
	name, policy, code string
	id                 int64
	reachable, io      bool
}

func TestPinDefaultDenyReachabilityAndProducerIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []pinCase{
		{"denied", "{}", "pin", 2, true, false},
		{"allowed", `{"origin/team/widget":["origin/team/source"]}`, "", 2, true, false},
		{"unreachable", `{"origin/team/widget":["origin/team/source"]}`, "reachable", 2, false, false},
		{"unregistered", `{"origin/team/widget":["origin/team/source"]}`, "reachable", 0, true, false},
		{"outage", `{"origin/team/widget":["origin/team/source"]}`, "unavailable", 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) { runPinCase(t, tc) })
	}
}
func TestSourcePolicyBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		at, over []byte
	}{{"bytes", []byte("{}" + strings.Repeat(" ", (64<<10)-2)), []byte("{}" + strings.Repeat(" ", (64<<10)-1))}}
	consumers := map[string][]string{}
	for i := range 256 {
		consumers[fmt.Sprintf("origin/team/repo%d", i)] = nil
	}
	raw, _ := json.Marshal(consumers)
	consumers["origin/team/extra"] = nil
	over, _ := json.Marshal(consumers)
	tests = append(tests, struct {
		name     string
		at, over []byte
	}{"consumers", raw, over})
	producer := map[string][]string{"origin/team/widget": make([]string, 64)}
	for i := range producer[request().Repository] {
		producer[request().Repository][i] = "origin/team/source"
	}
	raw, _ = json.Marshal(producer)
	producer[request().Repository] = append(producer[request().Repository], "origin/team/source")
	over, _ = json.Marshal(producer)
	tests = append(tests, struct {
		name     string
		at, over []byte
	}{"producers", raw, over})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.d.SourcePolicy = tc.at
			s, err := app.New(f.d)
			if err != nil {
				t.Fatal("N refused", err)
			}
			closeSubmitter(t, s)
			f.d.SourcePolicy = tc.over
			_, err = app.New(f.d)
			expectRefusal(t, err, "policy")
		})
	}
	for _, raw := range []string{`[]`, `{"bad":[]}`, `{"origin/team/widget":["bad"]}`} {
		f := newFixture(t)
		f.d.SourcePolicy = []byte(raw)
		_, err := app.New(f.d)
		expectRefusal(t, err, "policy")
	}
}

func runPinCase(t *testing.T, tc pinCase) {
	t.Helper()
	f := newFixture(t)
	f.d.SourcePolicy = []byte(tc.policy)
	w := workflow()
	sha := strings.Repeat("e", 40)
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/source", Commit: sha})
	f.setDocument(w)
	calls := 0
	f.source.Reachable = func(_ context.Context, r sourcecontract.Revision) (int64, bool, error) {
		calls++
		if r.Commit != sha {
			t.Errorf("pin changed %+v", r)
		}
		if tc.io {
			return 0, false, errors.New("producer unavailable")
		}
		return tc.id, tc.reachable, nil
	}
	p, err := f.new(t).Admit(context.Background(), request(), f.source)
	if tc.code != "" {
		expectRefusal(t, err, tc.code)
	} else {
		if err != nil {
			t.Fatal(err)
		}
		b := p.View().SourceBindings[0]
		if b.Revision.Commit != sha || b.RepositoryID != tc.id || b.AuthorizationDigest == "" {
			t.Fatal(b)
		}
	}
	if tc.name == "denied" && calls != 0 {
		t.Fatal("denied producer was contacted")
	}
}
