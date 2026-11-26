package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func TestBackupJobRetriesOnly503WithinItsBound(t *testing.T) {
	for _, row := range []struct {
		name  string
		codes []int
		wait  time.Duration
	}{
		{"busy-then-success", []int{503, 503, 200}, 3 * time.Second},
		{"busy-bound", []int{503, 503, 503, 503, 503, 503}, 31 * time.Second},
		{"forbidden", []int{403}, 0},
		{"other-failure", []int{500}, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, closed := 0, 0
				source := accountDatabase{config: installer.BackupJobConfig{SnapshotURL: "https://server.test/api/backup/database"}, http: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
					if calls >= len(row.codes) || r.Header.Get("Authorization") != "Bearer projection-fixture" {
						t.Fatal("backup exceeded its attempts or lost authentication")
					}
					code := row.codes[calls]
					calls++
					return &http.Response{StatusCode: code, Body: backupRetryBody{Reader: strings.NewReader("reply"), closed: &closed}}, nil
				})}}
				start := time.Now() //nolint:forbidigo // synctest owns this clock.
				response, err := source.databaseResponse(t.Context(), []byte("projection-fixture"))
				elapsed := time.Since(start) //nolint:forbidigo // synctest owns this clock.
				if err != nil || calls != len(row.codes) || elapsed != row.wait || closed != calls-1 || response.StatusCode != row.codes[calls-1] {
					t.Fatal("backup retry outcome or backoff differs", calls, closed, err)
				}
				_ = response.Body.Close()
			})
		})
	}
}

type backupRetryBody struct {
	io.Reader
	closed *int
}

func (b backupRetryBody) Close() error { *b.closed++; return nil }

func TestBackupJobBackoffStopsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		source := accountDatabase{config: installer.BackupJobConfig{SnapshotURL: "https://server.test/api/backup/database"}, http: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
			calls++
			cancel()
			return &http.Response{StatusCode: 503, Body: http.NoBody}, nil
		})}}
		response, err := source.databaseResponse(ctx, []byte("projection-fixture"))
		if response != nil {
			defer response.Body.Close()
		}
		if err == nil || calls != 1 {
			t.Fatal("cancelled backup kept retrying", calls, err)
		}
	})
}
