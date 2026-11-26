package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Only the in-cluster Job retries capacity refusals. Its six attempts wait
// 1, 2, 4, 8 and 16 seconds inside the Job's existing overall deadline.
const backupJobAttempts = 6

func (s accountDatabase) databaseResponse(ctx context.Context, jwt []byte) (*http.Response, error) {
	for attempt := range backupJobAttempts {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.SnapshotURL, strings.NewReader("{}"))
		if err != nil {
			return nil, backupAccountProblem()
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+string(jwt))
		response, err := s.http.Do(request)
		if err != nil {
			return nil, backupAccountProblem()
		}
		if response.StatusCode != http.StatusServiceUnavailable || attempt == backupJobAttempts-1 {
			return response, nil
		}
		_ = response.Body.Close()
		timer := time.NewTimer(time.Second << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, backupAccountProblem()
		case <-timer.C:
		}
	}
	panic("backup attempts exhausted")
}
