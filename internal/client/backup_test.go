package client

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDatabaseBackupUsesIndependentRouteAndDoesNotRetry(t *testing.T) {
	for _, status := range []int{200, 403, 503, 302} {
		calls := 0
		c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			body, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || r.URL.Path != "/api/admin/database-backup" || string(body) != "{}" || r.Header.Get("Content-Type") != "application/json" {
				t.Fatal("incorrect backup request", r.URL, string(body))
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader("canonical archive"))}, nil
		}), nil)
		stream, err := c.DatabaseBackup(t.Context())
		if calls != 1 || (err == nil) != (status == 200) {
			t.Fatal("backup response or retry", status, calls, err)
		}
		if stream != nil {
			body, readErr := io.ReadAll(stream)
			_ = stream.Close()
			if readErr != nil || string(body) != "canonical archive" {
				t.Fatal("archive stream changed", string(body), readErr)
			}
		}
	}
}
