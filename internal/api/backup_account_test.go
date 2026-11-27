package api_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

type accountSnapshotFunc func(context.Context, string) (io.ReadCloser, error)

func (f accountSnapshotFunc) Snapshot(ctx context.Context, _, token string) (io.ReadCloser, error) {
	return f(ctx, token)
}

func TestAccountSnapshotHasOneNativeRouteAndNoAdministratorAccess(t *testing.T) {
	calls := 0
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.AccountBackup = accountSnapshotFunc(func(_ context.Context, token string) (io.ReadCloser, error) {
			if token != "projection-fixture" {
				return nil, publicerr.New(publicerr.Forbidden, "Invalid account.", "Use the backup account.", nil)
			}
			calls++
			return io.NopCloser(strings.NewReader("canonical-backup-fixture")), nil
		})
	})
	w := newStreamWriter()
	r := httptest.NewRequest("POST", "/api/backup/database", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer projection-fixture")
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 200 || w.Body.String() != "canonical-backup-fixture" || calls != 1 {
		t.Fatal("native backup account route refused its stream", w.Code, calls)
	}
	for _, path := range []string{"/api/admin/database-backup", "/api/admin/version", "/mcp"} {
		r = httptest.NewRequest("POST", path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer projection-fixture")
		response := newStreamWriter()
		f.server.Handler().ServeHTTP(response, r)
		if response.Code != 401 {
			t.Fatal("snapshot credential entered an ordinary private route", path, response.Code)
		}
	}
	if calls != 1 {
		t.Fatal("another route invoked the account snapshot capability")
	}
}

func TestAccountSnapshotStopsWhenServerDrains(t *testing.T) {
	started := make(chan struct{})
	reader, writer := io.Pipe()
	defer writer.Close()
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.AccountBackup = accountSnapshotFunc(func(context.Context, string) (io.ReadCloser, error) {
			close(started)
			return reader, nil
		})
	})
	r := httptest.NewRequest("POST", "/api/backup/database", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer projection-fixture")
	joined := make(chan struct{})
	go func() { defer close(joined); f.server.Handler().ServeHTTP(newStreamWriter(), r) }()
	<-started
	f.server.BeginDrain()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("account snapshot survived server draining")
	}
}

// The account endpoint authenticates only the projected token; a client
// certificate, which identifies administrators elsewhere, is refused outright.
func TestAccountSnapshotRefusesClientCertificate(t *testing.T) {
	calls := 0
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.AccountBackup = accountSnapshotFunc(func(context.Context, string) (io.ReadCloser, error) {
			calls++
			return io.NopCloser(strings.NewReader("canonical-backup-fixture")), nil
		})
	})
	r := httptest.NewRequest("POST", "/api/backup/database", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer projection-fixture")
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	w := newStreamWriter()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 401 || calls != 0 || strings.Contains(w.Body.String(), "canonical-backup-fixture") {
		t.Fatal("account snapshot accepted a request carrying a client certificate", w.Code, calls)
	}
}

func TestAccountSnapshotRefusesOversizedTokenBeforeReview(t *testing.T) {
	calls := 0
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.AccountBackup = accountSnapshotFunc(func(context.Context, string) (io.ReadCloser, error) {
			calls++
			return nil, publicerr.New(publicerr.Forbidden, "Invalid account.", "Use the backup account.", nil)
		})
	})
	for _, size := range []int{64 << 10, 64<<10 + 1} {
		r := httptest.NewRequest("POST", "/api/backup/database", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", size))
		w := newStreamWriter()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		if size > 64<<10 && (w.Code != 401 || calls != 1) {
			t.Fatal("oversized backup token reached authentication", w.Code, calls)
		}
	}
	if calls != 1 {
		t.Fatal("a token at the cap was not reviewed", calls)
	}
}
