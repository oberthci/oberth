package secretstore_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestSnapshotInspectionChecksNativeContentsAndBounds(t *testing.T) {
	valid, err := fake.Snapshot([]byte("private-store-data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		raw   []byte
		limit int64
		valid bool
	}{{"valid", valid, 1 << 20, true}, {"opaque", []byte("private-store-data"), 1 << 20, false},
		{"truncated", valid[:len(valid)-1], 1 << 20, false}, {"limit", valid, 8, false},
		{"inner-checksum", changedNativeState(t, valid), 1 << 20, false}} {
		t.Run(tc.name, func(t *testing.T) {
			err := secretstore.InspectSnapshot(bytes.NewReader(tc.raw), tc.limit)
			if (err == nil) != tc.valid {
				t.Fatal("native snapshot validation accepted damaged state or rejected valid state", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-store-data") {
				t.Fatal("snapshot validation exposed private state")
			}
		})
	}
}

func changedNativeState(t *testing.T, valid []byte) []byte {
	t.Helper()
	in, err := gzip.NewReader(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(in)
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	plain = bytes.ReplaceAll(plain, []byte("private-store-data"), []byte("changed-store-data"))
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err = w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestSnapshotRestoreChecksBeforeStoreWrites(t *testing.T) {
	called := false
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: snapshotTransport(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.Method != "POST" || r.URL.Path != "/v1/sys/storage/raft/snapshot-force" || r.Header.Get("X-Vault-Token") != "temporary-target-admin" {
			t.Fatal("restore used unexpected endpoint or authority")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || secretstore.InspectSnapshot(bytes.NewReader(raw), 1<<20) != nil {
			t.Fatal("restore did not rewind the checked native archive")
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	valid, err := fake.Snapshot([]byte("private-store-data"))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.RestoreSnapshot(t.Context(), []byte("temporary-target-admin"), bytes.NewReader(changedNativeState(t, valid)), 1<<20); err == nil || called {
		t.Fatal("corrupted native snapshot reached the restore endpoint", err)
	}
	if err = client.RestoreSnapshot(t.Context(), []byte("temporary-target-admin"), bytes.NewReader(valid), 1<<20); err != nil || !called {
		t.Fatal("checked native snapshot was not restored", err)
	}
}
