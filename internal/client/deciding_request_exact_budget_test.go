package client

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestCompleteRequestAllowsExactByteBudget(t *testing.T) {
	envelope := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"_meta": apicontract.ClientMetadata(), "name": "issue_import", "arguments": apicontract.IssueImportArgs{JSONL: ""}}}
	empty, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{-1, 0, 1} {
		calls := 0
		c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) != apicontract.RequestBytes+extra {
				t.Fatal("request fixture size differs", len(body), err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{}}}`))}, nil
		}), nil)
		input := apicontract.IssueImportArgs{JSONL: strings.Repeat(" ", apicontract.RequestBytes-len(empty)+extra)}
		err := c.Invoke(t.Context(), "issue_import", input, nil)
		if extra <= 0 {
			if err != nil || calls != 1 {
				t.Fatal("bounded request refused", extra, calls, err)
			}
		} else if err == nil || calls != 0 || IsOutcomeUnknown(err) || publicerr.Classify(err).Code() != publicerr.InvalidInput {
			t.Fatal("oversized request reached transport or became uncertain", calls, err)
		}
	}
}
