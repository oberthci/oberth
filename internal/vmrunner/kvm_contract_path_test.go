package vmrunner

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestDecodeContractUSTARArtifactPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path string
		accept     bool
	}{
		{"name-limit", strings.Repeat("a", 100), true},
		{"name-overflow", strings.Repeat("a", 101), false},
		{"suffix-limit", "build/" + strings.Repeat("a", 100), true},
		{"suffix-overflow", "build/" + strings.Repeat("a", 101), false},
		{"prefix-limit", strings.Repeat("a", 155) + "/b", true},
		{"prefix-overflow", strings.Repeat("a", 156) + "/b", false},
		{"complete-path-limit", strings.Repeat("a", 155) + "/" + strings.Repeat("b", 99), true},
		{"closed-path-cap", strings.Repeat("a", 155) + "/" + strings.Repeat("b", 100), false},
		{"split-before-last-directory", strings.Repeat("a", 154) + "/dir/" + strings.Repeat("b", 96), true},
		{"no-fitting-directory-split", "a/" + strings.Repeat("b", 154) + "/c", false},
		{"traversal", "build/../" + strings.Repeat("a", 80), false},
		{"non-ascii", "build/é", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.ReplaceAll(kvmContractYAML(), BPFArtifact, tc.path)
			contract, err := DecodeContract([]byte(source))
			if (err == nil) != tc.accept {
				t.Fatalf("DecodeContract path length %d: %v; accept=%v", len(tc.path), err, tc.accept)
			}
			if !tc.accept {
				return
			}
			// Prove every admitted boundary path survives the exact header format
			// used by the harness, without PAX or GNU extensions or truncation.
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			if err := tw.WriteHeader(&tar.Header{Name: contract.Artifacts[0], Mode: 0444, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(&archive)
			h, err := tr.Next()
			if err != nil || h.Name != tc.path || h.Format != tar.FormatUSTAR {
				t.Fatalf("USTAR header=%+v, err=%v", h, err)
			}
			content, err := io.ReadAll(tr)
			if err != nil || string(content) != "x" {
				t.Fatalf("USTAR content=%q, err=%v", content, err)
			}
			if _, err := tr.Next(); err != io.EOF {
				t.Fatalf("unexpected extra entry: %v", err)
			}
		})
	}
}
