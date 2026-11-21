package sourceplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewZeros struct{}

func (reviewZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestReviewSecondArchiveByteBoundary(t *testing.T) {
	for _, kind := range []sourcecontract.Kind{sourcecontract.Main, sourcecontract.Pinned} {
		for _, delta := range []int64{0, 1} {
			testArchiveBoundary(t, kind, delta)

		}
	}
}
func testArchiveBoundary(t *testing.T, kind sourcecontract.Kind, delta int64) {
	t.Helper()
	lim := sourcedelivery.MaxMainArchiveBytes
	if kind == sourcecontract.Pinned {
		lim = sourcedelivery.MaxPinArchiveBytes
	}
	hash := sha256.New()
	if _, e := io.CopyN(hash, reviewZeros{}, lim+delta); e != nil {
		t.Fatal(e)
	}
	d := sourcecontract.Descriptor{Version: 1, Kind: kind}
	if kind == sourcecontract.Pinned {
		d.Repository = "origin/team/widget"
		d.Commit = strings.Repeat("a", 40)
	}
	req := sourcedelivery.LoadRequest{Locator: "run", Descriptor: d}
	index := 0
	bytes := int64(0)
	rec, e := sourcedelivery.EncodeReceipt(sourcedelivery.LoadReceipt{Version: 1, Locator: req.Locator, Descriptor: d, Stream: sourcedelivery.StreamReceipt{AdmissionDigest: strings.Repeat("a", 64), Payload: sourcedelivery.Payload{Kind: kind, Index: &index}, Digest: hex.EncodeToString(hash.Sum(nil)), Bytes: &bytes}})
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "result")
	response := &http.Response{Body: io.NopCloser(io.LimitReader(reviewZeros{}, lim+delta)), Trailer: http.Header{http.CanonicalHeaderKey(sourcedelivery.ReceiptTrailer): []string{rec}}}
	e = receive(context.Background(), response, req, path)
	if (e == nil) != (delta == 0) {
		t.Errorf("%s limit=%d delta=%d: %v", kind, lim, delta, e)
	}
	if delta == 0 {
		if e = os.Remove(path); e != nil {
			t.Fatal(e)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Error("residue", entries)
	}
	t.Logf("%s %d+%d observed", kind, lim, delta)
}
func TestReviewSecondRetryFloorBoundary(t *testing.T) {
	for _, v := range []string{"300", "301"} {
		if retryFloor(v, time.Unix(0, 0)) != 300*time.Second {
			t.Fatal(v)
		}
	}
}
