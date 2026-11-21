package sourcedelivery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewSecondWireByteAndCountBoundaries(t *testing.T) {
	for _, n := range []int{MaxLoadRequestBytes, MaxLoadRequestBytes + 1} {
		body := `{"locator":"r","descriptor":{"version":1,"kind":"main"}}`
		body += strings.Repeat(" ", n-len(body))
		_, e := DecodeRequest([]byte(body))
		if (e == nil) != (n == MaxLoadRequestBytes) {
			t.Errorf("request %d %v", n, e)
		}
	}
	for _, n := range []int{128, 129} {
		b, _ := json.Marshal(LoadRequest{Locator: strings.Repeat("a", n), Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}})
		_, e := DecodeRequest(b)
		if (e == nil) != (n == 128) {
			t.Errorf("locator %d %v", n, e)
		}
	}
	d := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}
	index := 0
	nbytes := int64(0)
	rec := LoadReceipt{Version: 1, Locator: "r", Descriptor: d, Stream: StreamReceipt{AdmissionDigest: strings.Repeat("a", 64), Payload: Payload{Kind: sourcecontract.Main, Index: &index}, Digest: strings.Repeat("b", 64), Bytes: &nbytes}}
	data, _ := json.Marshal(rec)
	for _, n := range []int{MaxReceiptBytes, MaxReceiptBytes + 1} {
		raw := string(data) + strings.Repeat(" ", n-len(data))
		e := VerifyReceipt(base64.RawURLEncoding.EncodeToString([]byte(raw)), LoadRequest{Locator: "r", Descriptor: d}, strings.Repeat("b", 64))
		if (e == nil) != (n == MaxReceiptBytes) {
			t.Errorf("receipt %d %v", n, e)
		}
	}
}

func TestReviewSecondBearerBoundary(t *testing.T) {
	for _, n := range []int{8192, 8193} {
		called := false
		s := New(Config{SourceClientBurst: 3, Authenticate: func(_ context.Context, _ string) (Caller, error) {
			called = true
			return Caller{}, &Failure{Code: "unauthenticated"}
		}})
		_, _, _, _ = s.authorize(context.Background(), "Bearer "+strings.Repeat("a", n-7), LoadRequest{})
		if called != (n == 8192) {
			t.Errorf("bearer %d authenticate=%v", n, called)
		}
	}
}

func TestPinReceiptIndexAccepts4Refuses5(t *testing.T) {
	for _, index := range []int{4, 5} {
		err := validatePayload(Payload{Kind: sourcecontract.Pinned, Index: &index}, sourcecontract.Pinned)
		if (err == nil) != (index == 4) {
			t.Fatal(index, err)
		}
	}
}

func TestLoadFailureWireNamesSizeLimitAndGone(t *testing.T) {
	for _, failure := range []*Failure{{Code: "too_large", Limit: 134217728}, {Code: "too_large", Limit: 4096, Unit: "files"}, {Code: "gone", Message: "tag moved since admission"}} {
		server := New(Config{})
		response := httptest.NewRecorder()
		server.failure(response, failure)
		expected := 413
		if failure.Code == "gone" {
			expected = 410
		}
		if response.Code != expected {
			t.Fatal(response.Code)
		}
		var wire ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Code != failure.Code || wire.Limit != failure.Limit || wire.Message != failure.Message || wire.Unit != failure.Unit {
			t.Fatal(wire)
		}
	}
}
