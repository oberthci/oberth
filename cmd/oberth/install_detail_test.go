package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func detailedInstallationRefusal() error {
	return publicerr.Classify(errors.Join(publicerr.New(publicerr.InvalidInput, "First input is invalid.", "Correct the first input.", nil), publicerr.New(publicerr.InvalidInput, "Second input is invalid.", "Correct the second input.", nil)))
}

func TestEveryInstallationCommandAcceptsRefusalDetail(t *testing.T) {
	for _, row := range installationCommands() {
		t.Run(row.name, func(t *testing.T) {
			app, _, diagnostic := fixture(t, fake.Bindings())
			dispatch := func(context.Context, installationRequest, io.Reader, io.Writer) error {
				return detailedInstallationRefusal()
			}
			app.install, app.secretExec = dispatch, dispatch
			args := append(strings.Fields(row.name), "--detail")
			switch row.name {
			case "secret put":
				args = append(args, "path")
			case "secretstore exec":
				args = append(args, "--", "true")
			}
			if code := app.run(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), "First input is invalid.") || !strings.Contains(diagnostic.String(), "Second input is invalid.") || strings.Contains(diagnostic.String(), "More detail is available") {
				t.Fatal("suggested detail is unavailable", code, diagnostic.String())
			}
		})
	}
}

func TestSecretPutShowsGrantProblemsWithDetailOnEitherSideOfPath(t *testing.T) {
	for _, tail := range [][]string{{"--detail", "registry/secret"}, {"registry/secret", "--detail"}} {
		b := fake.Bindings()
		b.AccessAllow.Handler = &fake.Handler[apicontract.AccessAllowArgs, apicontract.Grant]{Error: detailedInstallationRefusal()}
		writes := &fake.Handler[apicontract.SecretPutArgs, apicontract.Receipt]{}
		b.SecretPut.Handler = writes
		app, out, diagnostic := fixture(t, b)
		app.in = strings.NewReader("private-input-marker")
		args := append([]string{"secret", "put", "--repo", "origin/team/widget", "--file", ".oberth/build.yaml", "--step", "build"}, tail...)
		code := app.run(t.Context(), args)
		_, _, stored := writes.Observed()
		if code != 1 || stored != 0 || !strings.Contains(diagnostic.String(), "First input is invalid.") || !strings.Contains(diagnostic.String(), "Second input is invalid.") || strings.Contains(out.String()+diagnostic.String(), "private-input-marker") {
			t.Fatal("grant detail missing or secret escaped refusal", code, stored, out.String(), diagnostic.String())
		}
	}
}
