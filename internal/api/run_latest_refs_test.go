package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestLatestPerRefHTTPIsOptInAndPagesQueuedHeads(t *testing.T) {
	server, backend := testServer(t)
	backend.runPage = []model.Run{{ID: "new", QueueSequence: 4, Status: model.RunQueued}, {ID: "other", QueueSequence: 2, Status: model.RunQueued}}
	request := httptest.NewRequest(http.MethodGet, "/api/runs?latest_per_ref=1&limit=2", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !backend.runFilter.LatestPerRef || response.Header().Get("X-Oberth-Next-Before") != "2" {
		t.Fatal("latest heads cursor dropped unstarted runs", response.Code, response.Header(), backend.runFilter)
	}
	for _, url := range []string{"/api/runs?latest_per_ref=bogus", "/api/runs?latest_per_ref=1&view=history", "/api/runs?latest_per_ref=1&view=active"} {
		request := httptest.NewRequest(http.MethodGet, url, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatal("invalid latest-ref request accepted", url, response.Code)
		}
	}
}
