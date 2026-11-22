package main

import (
	"errors"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/releaseverify"
	"io"
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
	if address != "https://github.com/oberthci/oberth/releases/download/v1.2.3/release.json" {
		t.Fatal("installer uses a different public release address", address)
	}
}

const assetAddress = "https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sig=fixture"

// redirectChain answers every request with a redirect to the next target,
// and the last target with the release bytes.
func redirectChain(calls *[]string, targets ...string) testTransport {
	return func(r *http.Request) (*http.Response, error) {
		*calls = append(*calls, r.URL.String())
		if len(*calls) > len(targets) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("bytes")), Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {targets[len(*calls)-1]}}, Body: http.NoBody, Request: r}, nil
	}
}

func TestReleaseDownloadFollowsGitHubAssetRedirect(t *testing.T) {
	for _, targets := range [][]string{{assetAddress}, {releaseverify.Origin + "/v1.2.3/release.json", assetAddress, assetAddress}} {
		var calls []string
		files := releaseHTTPFiles{ctx: t.Context(), version: "v1.2.3", transport: redirectChain(&calls, targets...)}
		file, err := files.Open("release.json")
		if err != nil {
			t.Fatal("allowed redirect refused", err)
		}
		body, err := io.ReadAll(file)
		if err != nil || string(body) != "bytes" || len(calls) != len(targets)+1 || calls[len(calls)-1] != assetAddress {
			t.Fatal("asset redirect not followed", calls, err)
		}
		_ = file.Close()
	}
}

func TestReleaseDownloadRefusesRedirectAndNamesTarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []string
	}{
		{"other host", []string{"https://other.example.test/release"}},
		{"plain HTTP", []string{"http://release-assets.githubusercontent.com/asset"}},
		{"other repository", []string{"https://github.com/other/project/releases/download/v1.2.3/release.json"}},
		{"look-alike host", []string{"https://release-assets.githubusercontent.com.example.test/asset"}},
		{"asset then other host", []string{assetAddress, "https://other.example.test/release"}},
		{"too many redirects", []string{assetAddress, assetAddress, assetAddress, assetAddress}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			files := releaseHTTPFiles{ctx: t.Context(), version: "v1.2.3", transport: redirectChain(&calls, tc.targets...)}
			file, err := files.Open("release.json")
			if file != nil {
				_ = file.Close()
				t.Fatal("redirect opened a file")
			}
			target := tc.targets[len(tc.targets)-1]
			var problem *publicerr.Error
			if !errors.As(err, &problem) || len(calls) != len(tc.targets) || !strings.Contains(strings.Join(problem.Context, "\n"), target) {
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
