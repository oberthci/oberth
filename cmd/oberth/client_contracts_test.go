package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestValidationFindingsDoNotTruncateLongFileOrTemplate(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	file := strings.Repeat("long-file-", 12) + ".yaml"
	template := strings.Repeat("long-template-", 8)
	document := strings.ReplaceAll(starterWorkflow(), "build", template)
	document = strings.Replace(document, "      container:\n", "      http: {url: 'https://oberth.ci'}\n      container:\n", 1)
	if err := os.WriteFile(filepath.Join(app.directory, file), []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	if app.run(t.Context(), []string{"validate", file}) != 1 {
		t.Fatal("invalid workflow accepted")
	}
	text := diagnostic.String()
	// A long unbroken file name or template stays whole, never split or truncated.
	if !strings.Contains(text, file) || !strings.Contains(text, template) {
		t.Fatal("file path was split or truncated", text)
	}
	assertFindingOutputWidth(t, text)
}

// Prose fits 80 columns; only a single word too long for any line exceeds it, whole.
func assertFindingOutputWidth(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > 80 && len(strings.Fields(line)) > 1 {
			t.Fatal("finding exceeds display width", line)
		}
	}
}

func TestProbeStatusReasonsFitDisplay(t *testing.T) {
	for _, reason := range []string{repositoryProbeFailureReason, repositoryArchivedReason, gitssh.HostKeyMismatchReason} {
		if len([]rune(reason)) > 40 {
			t.Fatal("probe status reason exceeds display bound", reason)
		}
	}
}

func TestCheckoutValidatesSSHEndpoint(t *testing.T) {
	for _, endpoint := range []string{"ssh://git@oberth.local:" + strings.Repeat("0", 1<<20) + "22", "ssh://git:password@oberth.local:22", "ssh://git@oberth.local:0", "ssh://git@oberth.local:999999", "ssh://other@oberth.local:22", "ssh://git@oberth.local:22/path"} {
		if _, err := profileSSH(client.Profile{SSHURL: endpoint}, "https://oberth.local"); err == nil {
			t.Fatalf("invalid checkout endpoint accepted (%d bytes)", len(endpoint))
		}
	}
}

func TestInviteHelpDescribesEveryRepositoryOfUpstream(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"invite", "--help"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	if !strings.Contains(out.String(), "invite <upstream>") || !strings.Contains(out.String(), "every repository") {
		t.Fatal("help promises repository authority", out.String())
	}
}

func TestRepoAddTrustRetryUsesWireIdentifier(t *testing.T) {
	for _, tc := range []struct {
		reason, id string
		retry      bool
	}{
		{"Confirm this forge host key.", "forge-trust-unconfirmed", true},
		{forgeTrustReason, "", false},
		{forgeTrustReason, "another_refusal", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": true, "structuredContent": map[string]any{"code": "forbidden", "id": tc.id, "reason": tc.reason, "remedy": "Verify the offered fingerprint."}}})
			remote, err := client.New(client.Config{Options: client.Options{URL: "https://oberth.local"}, Now: fake.NewClock().Now, Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			err = remote.Invoke(t.Context(), "upstream_add", map[string]string{"name": "origin", "base_url": "ssh://forge.oberth.local/team"}, nil)
			err = repoAddTrustFailure(err, "ssh://forge.oberth.local/team/widget.git", "/private/known_hosts", "origin")
			var problem *publicerr.Error
			if !errors.As(err, &problem) {
				t.Fatal("wire refusal lost", err)
			}
			retry := strings.Contains(strings.Join(problem.Context, "\n"), "Retry: oberth repo add")
			if tc.retry && !strings.Contains(strings.Join(problem.Context, "\n"), "--upstream 'origin'") {
				t.Fatal("retry lost the selected upstream", problem.Context)
			}
			if retry != tc.retry {
				t.Fatal("retry selected by human message instead of identifier", problem.Context)
			}
		})
	}
}

func TestRepoAddSynopsisShowsTrustConfirmationFlags(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"repo", "add", "--help"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(text, "repo add <forge URL> [--known-hosts file] [--expected-host-fingerprint fingerprint] [--repin]") {
		t.Fatal("synopsis omits trust flags", text)
	}
}
