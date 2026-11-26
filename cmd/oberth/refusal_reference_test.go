package main

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestRepoVerifyPlainRefusalPrintsCorrelationReference(t *testing.T) {
	b := fake.Bindings()
	b.RepoVerify.Handler = &fake.Handler[apicontract.RepoVerifyArgs, apicontract.Repository]{Error: errors.New("private probe failure")}
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"repo", "verify", "origin/team/widget"}); code != 1 {
		t.Fatal("unexpected exit", code, out, diagnostic)
	}
	if !regexp.MustCompile(`(?m)^Reference: [a-f0-9]{32}$`).MatchString(diagnostic.String()) {
		t.Fatal("plain refusal omitted correlation reference", diagnostic)
	}
	if strings.Contains(diagnostic.String(), "private probe failure") || strings.Contains(diagnostic.String(), "--detail") {
		t.Fatal("refusal leaked cause or hid its reference", diagnostic)
	}
}
