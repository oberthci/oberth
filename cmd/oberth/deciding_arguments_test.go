package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestDecidingGrantRecipesRetainNamedFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want apicontract.AccessAllowArgs
	}{
		{"pvc", []string{"--kind", "pvc", "--tier", "ci", "source/team/widget", "build-cache"}, apicontract.AccessAllowArgs{Kind: "pvc", Tier: "ci", Repo: "source/team/widget", Claim: "build-cache"}},
		{"named-tuple", []string{"--repo", "source/team/widget", "--workflow", ".oberth/build.yaml", "--step", "build", "registry/key"}, apicontract.AccessAllowArgs{Repo: "source/team/widget", Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}},
		{"mixed-tuple", []string{"source/team/widget", "--workflow", ".oberth/build.yaml", "build", "registry/key"}, apicontract.AccessAllowArgs{Repo: "source/team/widget", Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.Bindings()
			h := &fake.Handler[apicontract.AccessAllowArgs, apicontract.Grant]{}
			b.AccessAllow.Handler = h
			app, _, diagnostic := fixture(t, b)
			if code := app.run(t.Context(), append([]string{"access", "allow"}, tc.args...)); code != 0 {
				t.Fatal(code, diagnostic.String())
			}
			inputs, _, calls := h.Observed()
			if calls != 1 || !reflect.DeepEqual(inputs[0], tc.want) {
				t.Fatalf("grant tuple=%+v want=%+v", inputs, tc.want)
			}
		})
	}
}

func TestDecidingBearerDigestConflictHasNoEffects(t *testing.T) {
	b := fake.Bindings()
	h := &fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]{}
	b.UplinkAdd.Handler = h
	app, _, diagnostic := fixture(t, b)
	path := filepath.Join(decidingDirectory(t), "bearer")
	args := []string{"uplink", "add", "--identity", "browser", "--ssh-public-key", "public", "--bearer-file", path, "--bearer-digest", strings.Repeat("a", 64)}
	if code := app.run(t.Context(), args); code != 2 {
		t.Fatal("conflicting bearer sources accepted", code, diagnostic.String())
	}
	_, _, calls := h.Observed()
	if calls != 0 {
		t.Fatal("conflict activated an identity", calls)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("conflict wrote bearer file", err)
	}
	if !strings.Contains(diagnostic.String(), "Choose --bearer-file or --bearer-digest, not both.\n") {
		t.Fatal(diagnostic.String())
	}
}

func TestDecidingInvitationLifetimeDefaultAndOverride(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{{"default", nil, "24h"}, {"explicit", []string{"--expires", "2h"}, "2h"}} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.Bindings()
			h := &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{Value: model.Invitation{Identity: "agent", ServerURL: "https://127.0.0.1", Scope: []string{"source"}}}
			b.UplinkInvite.Handler = h
			app, _, diagnostic := fixture(t, b)
			env, key := app.env, issuerKey(t, issuerSSH, issuerHostKeys)
			app.env = func(name string) string {
				if name == "OBERTH_CLIENT_KEY" {
					return key
				}
				return env(name)
			}
			directory := decidingDirectory(t)
			path := filepath.Join(directory, "invitation.json")
			args := append([]string{"invite", "source", "--out", path}, tc.flags...)
			if code := app.run(t.Context(), args); code != 0 {
				t.Fatal(code, diagnostic.String())
			}
			inputs, _, calls := h.Observed()
			if calls != 1 || inputs[0].Expires != tc.want || inputs[0].Identity != "agent" || !reflect.DeepEqual(inputs[0].Scope, []string{"source"}) {
				t.Fatalf("invitation=%+v want expiry %s", inputs, tc.want)
			}
		})
	}
}

func TestDecidingBearerFileOnlyForUplinkAdd(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	directory := decidingDirectory(t)
	path := filepath.Join(directory, "bearer")
	if code := app.run(t.Context(), []string{"repo", "list", "--bearer-file", path}); code != 2 {
		t.Fatal("bearer generation reached another command", code, diagnostic.String())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("invalid command generated a bearer", err)
	}
	want := "Command arguments are invalid.\n--bearer-file is only valid for uplink add\nRun oberth help repo list for this command.\nRun oberth help for command names and arguments.\n"
	if diagnostic.String() != want {
		t.Fatalf("bearer scope usage=%q want=%q", diagnostic.String(), want)
	}
}

func TestDecidingRawToolInputRequiresObject(t *testing.T) {
	for _, input := range []string{"null", "[]", "true", "{}"} {
		t.Run(input, func(t *testing.T) {
			b := fake.Bindings()
			h := &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{}
			b.RepoList.Handler = h
			app, _, diagnostic := fixture(t, b)
			code := app.run(t.Context(), []string{"tool", "repo_list", input})
			_, _, calls := h.Observed()
			if input == "{}" {
				if code != 0 || calls != 1 {
					t.Fatal("object input refused", code, calls, diagnostic.String())
				}
				return
			}
			want := "Command arguments are invalid.\ntool input must be a bounded JSON object\nRun oberth help repo list for this command.\nRun oberth help for command names and arguments.\n"
			if code != 2 || calls != 0 || diagnostic.String() != want {
				t.Fatalf("nonobject input: exit=%d calls=%d diagnostic=%q want=%q", code, calls, diagnostic.String(), want)
			}
		})
	}
}
