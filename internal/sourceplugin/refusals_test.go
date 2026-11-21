package sourceplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRefusalsNameSizeGoneAndMovedTag(t *testing.T) {
	for _, test := range []struct {
		code, message, contains string
		status                  int
		limit                   int64
	}{
		{"too_large", "", "128 MiB", 413, 134217728},
		{"too_large", "", "512 MiB", 413, 536870912},
		{"gone", "", "no longer kept", 410, 0},
		{"gone", "tag moved since admission", "tag moved since admission", 410, 0},
	} {
		response := &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"version":1,"code":%q,"message":%q,"limit":%d}`, test.code, test.message, test.limit)))}
		err := responseFailure(response)
		if err == nil || !strings.Contains(err.Error(), test.contains) {
			t.Fatal(test.code, err)
		}
	}
}
func TestArtifactLocatorAccepts128Refuses129(t *testing.T) {
	for _, n := range []int{128, 129} {
		_, err := input(&artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: strings.Repeat("a", n), Configuration: `{"version":1,"kind":"main"}`}})
		if (err == nil) != (n == 128) {
			t.Fatal(n, err)
		}
	}
}

func TestCompleteBodyAfterCancellationNeverReplacesDestination(t *testing.T) {
	req := sourcedelivery.LoadRequest{Locator: "run", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}}
	sum := sha256.Sum256([]byte("source"))
	index := 0
	n := int64(6)
	receipt, err := sourcedelivery.EncodeReceipt(sourcedelivery.LoadReceipt{Version: 1, Locator: "run", Descriptor: req.Descriptor, Stream: sourcedelivery.StreamReceipt{AdmissionDigest: strings.Repeat("a", 64), Payload: sourcedelivery.Payload{Kind: sourcecontract.Main, Index: &index}, Digest: hex.EncodeToString(sum[:]), Bytes: &n}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source")
	if err = os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("source")), Trailer: http.Header{http.CanonicalHeaderKey(sourcedelivery.ReceiptTrailer): []string{receipt}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = receive(ctx, response, req, path); err == nil {
		t.Fatal("cancelled body published")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "old" {
		t.Fatal("existing destination lost", err)
	}
}

func TestPinFileCountRefusalNamesFilesInsteadOfBytes(t *testing.T) {
	response := &http.Response{StatusCode: 413, Body: io.NopCloser(strings.NewReader(`{"version":1,"code":"too_large","limit":4096,"unit":"files"}`))}
	err := responseFailure(response)
	if err == nil || !strings.Contains(err.Error(), "4096 files") || strings.Contains(err.Error(), "4096 bytes") {
		t.Fatal("file limit mislabeled", err)
	}
}

func TestUnknownLimitUnitIsAProtocolFailure(t *testing.T) {
	response := &http.Response{StatusCode: 413, Body: io.NopCloser(strings.NewReader(`{"version":1,"code":"too_large","limit":4096,"unit":"entries"}`))}
	err := responseFailure(response)
	var accepted *loadRefusal
	if err == nil || errors.As(err, &accepted) {
		t.Fatal("unknown limit unit was accepted", err)
	}
}
