package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	apifake "github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func defaultIssueViewer(t *testing.T, real bool) *api.Server {
	t.Helper()
	p := issueUpstreamPeople(t, real)
	create, err := p.f.s.PrepareIssueCreate(t.Context(), adminCaller(), apicontract.IssueCreateArgs{Title: "Scoped administrator task", Body: new("Visible to the registered upstream.")})
	got, err := execute(t.Context(), create, err)
	if err != nil || got.Issue.Upstream != "origin" || got.ID != 1 {
		t.Fatal("issue did not choose the sole registered upstream", err)
	}
	viewer := workerCaller()
	viewer.CredentialID = "test-credential"
	return serviceHTTPServer(t, p.f, apifake.NewAuth(viewer))
}

func TestIssueCreateDefaultUpstreamReachesViewerAPIAndPage(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			server := defaultIssueViewer(t, real)
			assertDefaultIssueViewerHTTP(t, server)
		})
	}
}

func assertDefaultIssueViewerHTTP(t *testing.T, server *api.Server) {
	t.Helper()
	for _, path := range []string{"/api/issues/1", "/issues/1"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatal("viewer cannot open issue", path, response.Code, response.Body.String())
		}
		if path == "/api/issues/1" {
			var got model.Issue
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Upstream != "origin" || got.Title != "Scoped administrator task" {
				t.Fatal("wrong scoped issue", got.ID)
			}
		} else if !strings.Contains(response.Body.String(), "app.js") {
			t.Fatal("issue web page did not serve the application")
		}
	}
}

// TestUIExampleServer exposes the real creation and scoped read paths to browser assertions.
func TestUIExampleServer(t *testing.T) {
	address := os.Getenv("OBERTH_UI_EXAMPLE")
	if address == "" {
		t.Skip("browser fixture runs in ui-check")
	}
	handler := defaultIssueViewer(t, true)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = server.Close() }()
	t.Logf("Fixed examples at http://%s/issues/1", listener.Addr())
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}
