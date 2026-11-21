package main

import (
	"errors"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/releaseverify"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestInstallationDownloadsFromPublicReleaseAddress(t *testing.T) {
	address := ""
	files := releaseHTTPFiles{ctx: t.Context(), version: "v1.2.3", transport: testTransport(func(r *http.Request) (*http.Response, error) {
		address = r.URL.String()
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})}
	if file, err := files.Open("release.json"); err == nil {
		_ = file.Close()
		t.Fatal("fixture unexpectedly returned a release")
	}
	if address != "https://releases.oberth.ci/oberth/v1.2.3/release.json" {
		t.Fatal("installer uses a different public release address", address)
	}
}

func TestReleaseDownloadRefusesRedirectAndNamesTarget(t *testing.T) {
	for _, target := range []string{releaseverify.Origin + "/moved", "https://other.oberth.ci/release", "http://other.oberth.ci/release"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			files := releaseHTTPFiles{ctx: t.Context(), version: "v1.2.3", transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {target}}, Body: http.NoBody, Request: r}, nil
			})}
			file, err := files.Open("release.json")
			if file != nil {
				_ = file.Close()
				t.Fatal("redirect opened a file")
			}
			var problem *publicerr.Error
			if !errors.As(err, &problem) || calls != 1 || !strings.Contains(strings.Join(problem.Context, "\n"), target) {
				t.Fatal("redirect was followed or target hidden", calls, err)
			}
			app, _, diagnostic := fixture(t, fake.Bindings())
			if code := app.failed(installationFailure(installer.NewProblem("release", err, ""))); code != 1 || !strings.Contains(diagnostic.String(), target) {
				t.Fatal("installation renderer hid redirect target", code, diagnostic.String())
			}

		})
	}
}

func TestDocumentedPublicReleaseAddressMatchesConstant(t *testing.T) {
	for _, name := range []string{"install.md", "releasing.md"} {
		raw, err := os.ReadFile("../../docs/" + name)
		if err != nil || !strings.Contains(string(raw), releaseverify.Origin) {
			t.Fatal("documented address differs", name, err)
		}
	}
}

func TestInstallationFailureKeepsRollbackGuidanceAroundClientRefusal(t *testing.T) {
	cause := publicerr.New(publicerr.Unavailable, "The server refused the request.", "Check the server and retry.", nil)
	for _, code := range []string{"upgrade-failed", "backup"} {
		problem := installer.NewProblem(code, cause, "Record: /profile/rollback.json")
		got := installationFailure(problem)
		var public *publicerr.Error
		if !errors.As(got, &public) || public.Reason() != problem.Reason || public.Remedy() != problem.Remedy || strings.Join(public.Context, "\n") != problem.Detail {
			t.Fatal("installer guidance was replaced", got)
		}
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.failed(got)
		if !strings.Contains(strings.Join(strings.Fields(diagnostic.String()), " "), problem.Remedy) || !strings.Contains(diagnostic.String(), "Record:\n/profile/rollback.json") {
			t.Fatal("installer guidance was hidden", diagnostic.String())
		}
	}
}
