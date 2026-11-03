package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestIssueHTTPDetailServesFullBodyAndHonorsScope(t *testing.T) {
	f := newScopeFixture(t)
	body := strings.Repeat("complete body \u2603\n", 1000)
	visible, err := f.store.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "visible", Body: body, ScopeUpstreamID: f.github.ID})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.store.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "hidden", Body: "private body", ScopeUpstreamID: f.codeberg.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []api.Actor{scopedActor, scopedAdmin} {
		server, err := api.New(auditSurfaceAuth{actor}, f.service, f.service, "test", api.WithErrorClassifier(func(err error) (int, string) {
			if errors.Is(err, store.ErrNotFound) {
				return http.StatusNotFound, "not found"
			}
			return http.StatusInternalServerError, "internal error"
		}))
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			id     int64
			status int
		}{{visible.ID, 200}, {hidden.ID, 404}, {f.global.ID, 404}, {999999, 404}} {
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/issues/%d", tc.id), nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("scope=%+v id=%d: %d %s", actor, tc.id, response.Code, response.Body.String())
			}
			if tc.status == http.StatusOK {
				var value api.IssueResponse
				if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				if value.ID != visible.ID || value.Body != body {
					t.Fatalf("HTTP detail truncated body: got %d bytes, want %d", len(value.Body), len(body))
				}
			} else if strings.Contains(response.Body.String(), "private body") {
				t.Fatal("hidden body leaked")
			}
		}
	}
}
