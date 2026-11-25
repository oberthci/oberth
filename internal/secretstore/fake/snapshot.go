package fake

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Snapshot supplies native framing and checksums around private fixture state.
func Snapshot(state []byte) ([]byte, error) {
	meta, err := json.Marshal(map[string]any{"Version": 1, "ID": "fixture", "Index": 7, "Term": 1, "Size": len(state)})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.NoCompression)
	if err != nil {
		return nil, err
	}
	w := tar.NewWriter(gz)
	sums := fmt.Appendf(nil, "%x  meta.json\n%x  state.bin\n", sha256.Sum256(meta), sha256.Sum256(state))
	for _, item := range []struct {
		name string
		data []byte
	}{{"meta.json", meta}, {"state.bin", state}, {"SHA256SUMS", sums}, {"SHA256SUMS.sealed", []byte("sealed-fixture")}} {
		if err = w.WriteHeader(&tar.Header{Name: item.name, Mode: 0600, Size: int64(len(item.data))}); err != nil {
			return nil, err
		}
		if _, err = w.Write(item.data); err != nil {
			return nil, err
		}
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	if err = gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
