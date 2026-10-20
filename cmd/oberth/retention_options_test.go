package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/service"
)

func TestRetentionServeOptionsDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	options, err := parseServeOptions([]string{"--argo-namespace=pipelines"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.logRetention != 0 || options.artifactRetention != 0 || options.logsBudgetBytes != 0 || options.artifactsBudgetBytes != 4<<30 || options.retentionInterval != 10*time.Minute || options.retentionKeepNewest != 1 {
		t.Fatalf("retention defaults=%#v", options)
	}
	options, err = parseServeOptions([]string{"--argo-namespace=pipelines", "--log-retention=720h", "--artifact-retention=168h", "--logs-budget-bytes=1024", "--artifacts-budget-bytes=2048", "--artifacts-limit-bytes=512", "--retention-sweep-interval=5m", "--retention-keep-newest=0"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.logRetention != 720*time.Hour || options.artifactRetention != 168*time.Hour || options.logsBudgetBytes != 1024 || options.artifactsBudgetBytes != 2048 || options.artifactsLimitBytes != 512 || options.retentionInterval != 5*time.Minute || options.retentionKeepNewest != 0 {
		t.Fatalf("retention overrides=%#v", options)
	}
	for _, argument := range []string{"--log-retention=-1h", "--artifact-retention=-1h", "--logs-budget-bytes=-1", "--artifacts-budget-bytes=-1", "--artifacts-limit-bytes=-1", "--artifacts-limit-bytes=0", "--retention-sweep-interval=0s", "--retention-keep-newest=-1"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=pipelines", argument}, io.Discard); err == nil {
			t.Fatalf("accepted %s", argument)
		}
	}
}

func TestPurgedEvidenceReturnsGoneWithItsExplanation(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: logs purged by owner on 2026-10-01T12:00:00Z", service.ErrEvidencePurged)
	code, message := classifyViewError(err)
	if code != http.StatusGone || !strings.Contains(message, "purged by owner") {
		t.Fatalf("purged response=%d %q", code, message)
	}
}
