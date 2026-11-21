package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRenderFactsAndUnchangedJSON(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	data := []byte(`{"state":"failed","reason":"image pull failed","detail":"full native diagnostic","steps":[{"reason":"waiting for a node"}]}`)
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "full native") || !strings.Contains(out.String(), "image pull failed") {
		t.Fatal("detail leaked or reason missing")
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 80 || strings.Contains(line, "...") {
			t.Fatal("status line is not readable whole")
		}
	}
	out.Reset()
	if err := app.render(data, false, commonOptions{detail: true}); err != nil || !strings.Contains(out.String(), "full native diagnostic") {
		t.Fatal("detail missing")
	}
	out.Reset()
	if err := app.render(data, false, commonOptions{json: true}); err != nil || !bytes.Equal(out.Bytes(), append(data, '\n')) || diagnostic.Len() != 0 {
		t.Fatal("JSON changed")
	}
}

func TestScheduleListShowsMissingBranchRemedy(t *testing.T) {
	b := fake.Bindings()
	b.ScheduleList.Handler = &fake.Handler[apicontract.ScheduleListArgs, apicontract.SchedulePage]{Value: apicontract.SchedulePage{Schedules: []apicontract.Schedule{{
		Repo: "origin/team/widget", Entry: "nightly", DisplayReason: "schedule branch not found",
		Detail: "schedule branch not found - Push the branch to the oberth remote: git push oberth <branch>.",
	}}}}
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"schedule", "list"}); code != 0 || diagnostic.Len() != 0 {
		t.Fatal(code, diagnostic.String())
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"schedule branch not found", "Push the branch to the oberth remote", "git push oberth <branch>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing schedule guidance %q: %s", want, out.String())
		}
	}
}

func TestRefusalUsesServerWordsAndExit(t *testing.T) {
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Error: publicerr.New(publicerr.Forbidden, "Repository access refused.", "Ask the operator for access.", nil)}
	app, out, diagnostic := fixture(t, b)
	if code := app.run(context.Background(), []string{"repo", "list"}); code != 1 || diagnostic.String() != "Repository access refused.\nAsk the operator for access.\n" || out.Len() != 0 {
		t.Fatalf("refusal changed: %d %s", code, diagnostic.String())
	}
	out.Reset()
	diagnostic.Reset()
	if app.run(context.Background(), []string{"repo", "list", "-o", "json"}) != 1 || !strings.Contains(out.String(), `"reason":"Repository access refused."`) {
		t.Fatal("JSON refusal missing")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func TestOutputFailure(t *testing.T) {
	app, _, _ := fixture(t, fake.Bindings())
	app.out = brokenWriter{}
	if app.run(context.Background(), []string{"repo", "list", "-o", "json"}) != 1 {
		t.Fatal("output failure succeeded")
	}
}

func TestMessageCatalogue(t *testing.T) {
	for key, m := range messages() {
		if publicerr.CheckMessage(m[0], 80) != nil || publicerr.CheckMessage(m[1], 120) != nil {
			t.Fatal("overlong message", key)
		}
	}
}
