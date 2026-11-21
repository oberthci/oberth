package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestCompleteRequestBudgetRefusesBeforeTransport(t *testing.T) {
	q := apicontract.IssueImportArgs{JSONL: strings.Repeat(" ", apicontract.RequestBytes-60)}
	raw, err := json.Marshal(q)
	if err != nil || len(raw) > apicontract.RequestBytes {
		t.Fatal("arguments fixture", len(raw), err)
	}
	calls := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, nil }), nil)
	err = c.Invoke(t.Context(), "issue_import", q, nil)
	if calls != 0 || err == nil || IsOutcomeUnknown(err) || publicerr.Classify(err).Code() != publicerr.InvalidInput {
		t.Fatal("oversized frame was sent or called uncertain", calls, err)
	}
}

func TestCancelledMutationBeforeSendHasKnownOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, nil }), nil)
	err := c.Invoke(ctx, "issue_import", apicontract.IssueImportArgs{JSONL: "file"}, nil)
	if err == nil || calls != 0 || IsOutcomeUnknown(err) {
		t.Fatal("unsent mutation called uncertain", calls, err)
	}
}
