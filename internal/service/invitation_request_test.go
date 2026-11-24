package service_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
)

type reviewAddressTransport func(*http.Request) (*http.Response, error)

func (f reviewAddressTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type invitationAddressCase struct {
	name, public, address, wantPath string
	allowed                         bool
}

func TestReviewInvitationAddressMatchesActualRequest(t *testing.T) {
	for _, tc := range []invitationAddressCase{
		{"root", "https://ci.example", "https://ci.example", "/mcp/enroll", true},
		{"root-to-mcp", "https://ci.example", "https://ci.example/mcp", "/mcp/enroll", true},
		{"mcp-to-root", "https://ci.example/mcp", "https://ci.example", "/mcp/enroll", true},
		{"prefix-to-mcp", "https://ci.example/prefix", "https://ci.example/prefix/mcp", "/prefix/mcp/enroll", true},
		{"mcp-to-prefix", "https://ci.example/prefix/mcp", "https://ci.example/prefix", "/prefix/mcp/enroll", true},
		{"default-port", "https://ci.example", "https://CI.EXAMPLE:443/", "/mcp/enroll", true},
		{"default-port-omitted", "https://ci.example:443/mcp", "https://ci.example/mcp/", "/mcp/enroll", true},
		{"trailing-slashes", "https://ci.example/mcp/", "https://ci.example/mcp///", "/mcp/enroll", true},
		{"prefix", "https://ci.example/prefix", "https://ci.example/prefix/", "/prefix/mcp/enroll", true},
		{"prefix-mcp", "https://ci.example/prefix/mcp", "https://ci.example/prefix/mcp/", "/prefix/mcp/enroll", true},
		{"ipv6", "https://[::1]/mcp", "https://[::1]:443/mcp/", "/mcp/enroll", true},
		{"userinfo", "https://ci.example/mcp", "https://person@ci.example/mcp", "", false},
		{"other-port", "https://ci.example/mcp", "https://ci.example:444/mcp", "", false},
		{"other-path", "https://ci.example/mcp", "https://ci.example/other", "", false},
		{"query", "https://ci.example/mcp", "https://ci.example/mcp?x=y", "", false},
		{"fragment", "https://ci.example/mcp", "https://ci.example/mcp#fragment", "", false},
		{"empty-query", "https://ci.example/mcp", "https://ci.example/mcp?", "/mcp/enroll", false},
		{"empty-fragment", "https://ci.example/mcp", "https://ci.example/mcp#", "/mcp/enroll", false},
	} {
		t.Run(tc.name, func(t *testing.T) { checkInvitationRequest(t, tc) })
	}
}

func checkInvitationRequest(t *testing.T, tc invitationAddressCase) {
	t.Helper()

	p := people(t)
	p.f.c.People.PublicURL = tc.public
	store := &countedInvitations{EnrollmentStore: p.state}
	p.f.c.People.Enrollment = store
	p.f.restart(t)
	q := apicontract.UplinkInviteArgs{Scope: []string{"*"}, Identity: "review", ServerURL: tc.address, SecretDigest: digestText(strings.Repeat("s", 48))}
	before := p.f.seq.Load()
	call, err := p.f.s.PrepareUplinkInvite(t.Context(), adminCaller(), q)
	if err != nil {
		if store.created != 0 || p.f.seq.Load() != before {
			t.Fatal("refused address allocated or stored invitation")
		}
		if tc.allowed {
			t.Fatalf("legitimate address refused: %v", err)
		}
		return
	}
	invitation, err := execute(t.Context(), call, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uri string
	cfg := client.Config{Now: p.f.c.Now, Transport: reviewAddressTransport(func(r *http.Request) (*http.Response, error) {
		uri = r.URL.RequestURI()
		return nil, io.ErrUnexpectedEOF
	})}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	_, joinErr := client.Join(t.Context(), cfg, model.InvitationFile{Invitation: invitation, KnownHosts: "[ci.example]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB\n", Secret: strings.Repeat("s", 48)}, filepath.Join(parent, "profile"))
	t.Logf("accepted address %q -> request %q; join: %v", tc.address, uri, joinErr)
	if !tc.allowed {
		t.Errorf("invalid delimiter address was registered; request %q", uri)
	}
	if uri != tc.wantPath {
		t.Errorf("registered invitation reaches %q, want %q", uri, tc.wantPath)
	}
}
