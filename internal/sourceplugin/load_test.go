package sourceplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"io"
	"net/http"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func identity(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("bound-token"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIncompleteTransferNeverPublishes(t *testing.T) {
	request := sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}}
	sum := sha256.Sum256([]byte("archive"))
	n := int64(7)
	index := 0
	receipt := sourcedelivery.LoadReceipt{Version: 1, Locator: request.Locator, Descriptor: request.Descriptor, Stream: sourcedelivery.StreamReceipt{AdmissionDigest: strings.Repeat("a", 64), Payload: sourcedelivery.Payload{Kind: sourcecontract.Main, Index: &index}, Digest: hex.EncodeToString(sum[:]), Bytes: &n}}
	good, err := sourcedelivery.EncodeReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		trailers   []string
		want       bool
	}{
		{"complete", "archive", []string{good}, true}, {"missing", "archive", nil, false}, {"wrong bytes", "different", []string{good}, false}, {"duplicate", "archive", []string{good, good}, false}, {"invalid", "archive", []string{"broken"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.tgz")
			if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
			response := &http.Response{Body: io.NopCloser(strings.NewReader(tc.body)), Trailer: http.Header{http.CanonicalHeaderKey(sourcedelivery.ReceiptTrailer): tc.trailers}}
			err := receive(t.Context(), response, request, path)
			if (err == nil) != tc.want {
				t.Fatalf("result %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "existing"
			if tc.want {
				want = "archive"
			}
			if string(body) != want {
				t.Fatalf("published %q", body)
			}
		})
	}
}
func TestRetryOnly503AndHonorsFloor(t *testing.T) {
	now := func() time.Time { return time.Unix(100, 0) }
	var delays []time.Duration
	calls := 0
	c := &Client{tokenFile: identity(t), endpoint: "https://source.invalid", now: now, jitter: func(d time.Duration) time.Duration { return d }, wait: func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) > 3 {
			return errors.New("unexpected extra retry")
		}
		return nil
	}}
	c.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		status := 503
		if calls == 4 {
			status = 403
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private dependency body")), Header: http.Header{"Retry-After": []string{"1"}}}, nil
	})}
	r, err := c.response(t.Context(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if calls != 4 || len(delays) != 3 || delays[0] != 2*time.Second || delays[1] != 3*time.Second || delays[2] != 3*time.Second {
		t.Fatalf("%d %v", calls, delays)
	}
	if retryFloor("900", now()) != 5*time.Minute || retryFloor("-1", now()) != 0 {
		t.Fatal("retry floor bounds")
	}
}
