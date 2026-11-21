package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestUnsealPrintsOneClosingOutcome(t *testing.T) {
	for _, name := range []string{"sealed", "already-unsealed", "failed"} {
		t.Run(name, func(t *testing.T) { checkUnsealOutcome(t, name) })
	}
}

func checkUnsealOutcome(t *testing.T, name string) {
	t.Helper()
	f := signedMaintenanceFixture(t)
	backend := &storefake.Store{Initialized: true, Sealed: name != "already-unsealed"}
	if name == "failed" {
		backend.Fail = "/sys/unseal"
	}
	f.session.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
		return c, func() {}, err
	}
	f.session.request.name = "unseal"
	var out bytes.Buffer
	err := f.session.dispatch(t.Context(), nil, &out)
	if name == "failed" {
		checkUnsealFailure(t, err, &out)
		return
	}
	want := "Secret store unsealed.\n"
	if name == "already-unsealed" {
		want = "Secret store was already unsealed.\n"
		if len(backend.Calls) != 1 || backend.Calls[0].Path != "/sys/seal-status" {
			t.Fatal("already-unsealed store was changed", backend.Calls)
		}
	}
	if err != nil || out.String() != want || backend.Sealed || strings.Count(out.String(), "\n") != 1 {
		t.Fatal("unseal outcome is silent or incorrect", err, out.String())
	}
}

func checkUnsealFailure(t *testing.T, err error, out *bytes.Buffer) {
	t.Helper()
	app, _, diagnostic := fixture(t, fake.Bindings())
	problem := publicerr.Classify(installationFailure(err))
	if err == nil || out.Len() != 0 || app.failed(problem) != 1 || problem.Remedy() == "" || diagnostic.Len() == 0 {
		t.Fatal("failed unseal has no refusal or prints success", err, out.String())
	}
}
