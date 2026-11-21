package sourceplugin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestSourceLoadHeadersMustDescribeAnUnencodedArchive(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, encoding string
		want                        bool
	}{
		{"archive", sourcedelivery.LoadContentType, "", true},
		{"another-type", "application/octet-stream", "", false},
		{"encoded", sourcedelivery.LoadContentType, "gzip", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := ruleLoadClient(t, func(*http.Request) (*http.Response, error) {
				response := ruleArchiveResponse(t)
				response.Header.Set("Content-Type", tc.contentType)
				response.Header.Set("Content-Encoding", tc.encoding)
				return response, nil
			})
			path := filepath.Join(t.TempDir(), "source.tgz")
			identityOK(t, os.WriteFile(path, []byte("existing"), 0600))
			err := client.Load(t.Context(), ruleArtifact(), path)
			want := "existing"
			if tc.want {
				want = "archive"
			}
			body, readErr := os.ReadFile(path)
			if (err == nil) != tc.want || readErr != nil || string(body) != want {
				t.Fatalf("load error %v, destination %q, read error %v", err, body, readErr)
			}
			entries, readErr := os.ReadDir(filepath.Dir(path))
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("private archive remained after load: %v, %v", entries, readErr)
			}
		})
	}
}

func TestSourceLoadDoesNotRetryOtherServerErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, waits := 0, 0
			client := ruleLoadClient(t, func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("unavailable upstream"))}, nil
			})
			client.wait = func(context.Context, time.Duration) error { waits++; return context.Canceled }
			path := filepath.Join(t.TempDir(), "source.tgz")
			identityOK(t, os.WriteFile(path, []byte("existing"), 0600))
			err := client.Load(t.Context(), ruleArtifact(), path)
			body, readErr := os.ReadFile(path)
			if err == nil || calls != 1 || waits != 0 || readErr != nil || string(body) != "existing" {
				t.Fatalf("server error retried or published: %v, calls %d, waits %d, destination %q", err, calls, waits, body)
			}
		})
	}
}

func TestSourceLoadReopensProjectedTokenAfterRetryWait(t *testing.T) {
	directory := t.TempDir()
	for _, version := range []string{"first", "second"} {
		identityOK(t, os.Mkdir(filepath.Join(directory, version), 0700))
		identityOK(t, os.WriteFile(filepath.Join(directory, version, "token"), []byte(version), 0600))
	}
	identityOK(t, os.Symlink("first", filepath.Join(directory, "..data")))
	identityOK(t, os.Symlink("..data/token", filepath.Join(directory, "token")))
	calls, waits := 0, 0
	client := ruleLoadClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		want := "Bearer first"
		if calls == 2 {
			want = "Bearer second"
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("attempt %d did not use the current projected token", calls)
		}
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(http.NoBody)}, nil
		}
		return ruleArchiveResponse(t), nil
	})
	client.tokenFile = filepath.Join(directory, "token")
	client.wait = func(context.Context, time.Duration) error {
		waits++
		if waits > 1 {
			return errors.New("unexpected additional retry")
		}
		identityOK(t, os.Symlink("second", filepath.Join(directory, "..next")))
		return os.Rename(filepath.Join(directory, "..next"), filepath.Join(directory, "..data"))
	}
	path := filepath.Join(t.TempDir(), "source.tgz")
	err := client.Load(t.Context(), ruleArtifact(), path)
	body, readErr := os.ReadFile(path)
	if err != nil || calls != 2 || waits != 1 || readErr != nil || string(body) != "archive" {
		t.Fatalf("rotated token did not complete load: %v, calls %d, waits %d, destination %q", err, calls, waits, body)
	}
}

func ruleArtifact() *artifact.Artifact {
	return &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run", Configuration: `{"version":1,"kind":"main"}`}}
}

func ruleArchiveResponse(t *testing.T) *http.Response {
	t.Helper()
	index, size := 0, int64(7)
	receipt, err := sourcedelivery.EncodeReceipt(sourcedelivery.LoadReceipt{Version: 1, Locator: "run", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, Stream: sourcedelivery.StreamReceipt{AdmissionDigest: strings.Repeat("a", 64), Payload: sourcedelivery.Payload{Kind: sourcecontract.Main, Index: &index}, Digest: sourcecontract.Digest([]byte("archive")), Bytes: &size}})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {sourcedelivery.LoadContentType}}, Body: io.NopCloser(strings.NewReader("archive")), Trailer: http.Header{http.CanonicalHeaderKey(sourcedelivery.ReceiptTrailer): {receipt}}}
}

func ruleLoadClient(t *testing.T, transport roundTrip) *Client {
	t.Helper()
	client := &Client{tokenFile: identity(t), endpoint: "https://source.invalid", http: &http.Client{Transport: transport}, now: func() time.Time { return time.Unix(0, 0) }, jitter: func(time.Duration) time.Duration { return time.Nanosecond }}
	client.wait = func(context.Context, time.Duration) error { return errors.New("unexpected retry") }
	t.Cleanup(client.Close)
	return client
}
