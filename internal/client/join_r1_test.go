package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedJoinLeavesNoProfileOrReservation(t *testing.T) {
	for _, kind := range []string{"dial", "pin", "refused", "used", "lost", "scope", "expired"} {
		t.Run(kind, func(t *testing.T) { failedJoinCase(t, kind) })
	}
}
func failedJoinCase(t *testing.T, kind string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "agent")
	f := newEnrollment(t, false, dir)
	want := "invitation status is unknown"
	switch kind {
	case "dial":
		want = "invitation is still valid"
		f.config.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") })
	case "pin":
		want = "does not match the invitation"
		f.config.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return nil, failure("join_peer", nil) })
	case "refused", "used":
		reason := "Enrollment refused."
		if kind == "used" {
			reason = "Invitation used."
		}
		want = "Invitation redemption was refused."
		if kind == "used" {
			want = "Invitation used."
			f.config.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
				raw := `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"data":{"code":"forbidden","reason":"Invitation used.","remedy":"Ask for a new invitation."}}}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
			})
			break
		}
		h := server(t, fake.Bindings(), &fake.Handler[model.InviteRedemption, model.InviteCertificate]{PrepareError: publicerr.New(publicerr.Forbidden, reason, "Check the invitation with the administrator.", nil)})
		f.config.Transport = handlerTransport(h)
	case "lost":
		f.config.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			httptrace.ContextClientTrace(r.Context()).WroteRequest(httptrace.WroteRequestInfo{Err: io.ErrUnexpectedEOF})
			return nil, io.ErrUnexpectedEOF
		})
	case "scope":
		original := f.config.Transport
		f.config.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			resp, err := original.RoundTrip(r)
			if err != nil {
				return resp, err
			}
			return wrongScopeReply(t, resp), nil
		})
	case "expired":
		want = "Invitation or enrollment certificate is invalid"
		f.invitation.ExpiresAt = f.config.Now()
	}
	assertFailedJoin(t, f, joinFailureCase{kind, want, base, dir})
}

type joinFailureCase struct{ kind, want, base, dir string }

func assertFailedJoin(t *testing.T, f enrollmentFixture, c joinFailureCase) {
	t.Helper()
	kind, want, base, dir := c.kind, c.want, c.base, c.dir
	for range 2 {
		_, err := Join(t.Context(), f.config, f.invitation, dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", kind, err)
		}
		if kind != "used" && strings.Contains(err.Error(), "Invitation used") {
			t.Fatal("client invented consumption")
		}
		switch kind {
		case "refused", "used":
			if strings.Contains(err.Error(), "still valid") {
				t.Fatal("client invented invitation validity", err)
			}
		}
		if kind == "used" && !strings.Contains(err.Error(), "Ask for a new invitation.") {
			t.Fatal("server remedy was lost", err)
		}
		assertEmptyJoinBase(t, base)
	}
	if kind == "expired" && *f.calls != 0 {
		t.Fatal("expired invitation reached server")
	}
}

func wrongScopeReply(t *testing.T, resp *http.Response) *http.Response {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if json.Unmarshal(data, &wire) != nil {
		t.Fatal("invalid fixture reply")
	}
	result := wire["result"].(map[string]any)
	structured := result["structuredContent"].(map[string]any)
	structured["scope"] = []string{"another"}
	raw, _ := json.Marshal(structured)
	result["content"] = []any{map[string]any{"type": "text", "text": string(raw)}}
	data, _ = json.Marshal(wire)
	resp.Body = io.NopCloser(strings.NewReader(string(data)))
	return resp
}
func assertEmptyJoinBase(t *testing.T, base string) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed join left files: %v %v", entries, err)
	}
}
func TestInviteNeverOverwritesHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(path, []byte("existing handoff")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF }), nil)
	_, err := c.Invite(context.Background(), apicontract.UplinkInviteArgs{}, path, fixtureHostKeys)
	raw, readErr := ReadPrivate(path)
	if err == nil || readErr != nil || string(raw) != "existing handoff" || calls != 0 {
		t.Fatal("existing handoff overwritten or sent")
	}
}
func TestJoinScopeMustMatchInvitation(t *testing.T) {
	keys, err := generateKeys("agent")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, 98)
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := issueEnrollment(t, model.InviteRedemption{CSR: string(keys.csr)}, enrollmentSigner{pair: pair})
	invitation := model.InvitationFile{Invitation: model.Invitation{Identity: "agent", Scope: []string{"other"}, CAPin: func() string { ca, _ := clientcert.ParseCertificate(cert); return clientcert.Pin(ca) }()}}
	if err := verifyEnrolled(result, invitation, enrolledMaterial{keys, cert, fake.NewClock().Now()}); err == nil {
		t.Fatal("foreign scope accepted")
	}
}

func TestJoinNeverReplacesDestinationCreatedDuringRedemption(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "agent")
	f := newEnrollment(t, false, dir)
	original := f.config.Transport
	f.config.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if err != nil {
			return response, err
		}
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		return response, nil
	})
	if _, err := Join(t.Context(), f.config, f.invitation, dir); err == nil {
		t.Fatal("join replaced an occupied profile destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("occupied destination was changed", entries, err)
	}
	stages, _ := filepath.Glob(filepath.Join(base, ".join-*"))
	if len(stages) != 0 {
		t.Fatal("failed publication left staged files")
	}
}
