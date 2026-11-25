package secretstore

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// InspectSnapshot parses the native archive and verifies its internal checksums.
// The encrypted store state stays opaque; the outer age stream authenticates custody.
func InspectSnapshot(in io.Reader, limit int64) error {
	if in == nil || limit <= 0 {
		return refuse("input", nil)
	}
	gz, err := gzip.NewReader(in)
	if err != nil {
		return refuse("response", nil)
	}
	defer gz.Close()
	bounded := &io.LimitedReader{R: gz, N: limit + (64 << 10) + 1}
	reader := tar.NewReader(bounded)
	inspection := nativeSnapshot{digests: map[string]string{}}
	for range 4 {
		if err = inspection.member(reader, limit); err != nil {
			return refuse("response", nil)
		}
	}
	_, end := reader.Next()
	_, err = io.Copy(io.Discard, bounded) // Check gzip checksum and EOF too.
	if !errors.Is(end, io.EOF) || err != nil || bounded.N == 0 || inspection.size != inspection.stateSize || !snapshotSums(inspection.sums, inspection.digests) {
		return refuse("response", nil)
	}
	return nil
}

type nativeSnapshot struct {
	digests         map[string]string
	sums            string
	size, stateSize int64
}

func (s *nativeSnapshot) member(reader *tar.Reader, limit int64) error {
	header, err := reader.Next()
	if err != nil || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > limit {
		return refuse("response", nil)
	}
	if _, exists := s.digests[header.Name]; exists {
		return refuse("response", nil)
	}
	digest := sha256.New()
	part := io.TeeReader(reader, digest)
	switch header.Name {
	case "meta.json":
		s.size, err = snapshotMetadata(part)
	case "state.bin":
		s.stateSize, err = io.Copy(io.Discard, part)
	case "SHA256SUMS":
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(part, 4097))
		if len(raw) > 4096 {
			return refuse("response", nil)
		}
		s.sums = string(raw)
	case "SHA256SUMS.sealed":
		_, err = io.Copy(io.Discard, part)
	default:
		return refuse("response", nil)
	}
	if err != nil {
		return refuse("response", nil)
	}
	s.digests[header.Name] = hex.EncodeToString(digest.Sum(nil))
	return nil
}

func snapshotMetadata(in io.Reader) (int64, error) {
	raw, err := io.ReadAll(io.LimitReader(in, (64<<10)+1))
	var meta struct {
		Version int
		ID      string
		Size    int64
		Index   uint64
		Term    uint64
	}
	if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &meta) != nil || meta.Version != 1 || meta.ID == "" || meta.Size <= 0 || meta.Index == 0 || meta.Term == 0 {
		return 0, refuse("response", nil)
	}
	return meta.Size, nil
}

func snapshotSums(sums string, digests map[string]string) bool {
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || seen[fields[1]] || (fields[1] != "meta.json" && fields[1] != "state.bin") || fields[0] != digests[fields[1]] {
			return false
		}
		seen[fields[1]] = true
	}
	return len(seen) == 2 && len(digests) == 4
}
