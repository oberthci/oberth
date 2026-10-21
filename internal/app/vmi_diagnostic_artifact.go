package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

// Merge before the existing extraction/secret gate, so one atomic collection
// includes both user artifacts and the server's bounded VM failure projection.
func appendVMIDiagnosticArtifact(archive, diagnostic []byte, limit int64) ([]byte, error) {
	const name = "oberth/vmi-diagnostics.json"
	if len(diagnostic) > 64<<10 || int64(len(diagnostic)) > limit {
		return nil, errors.New("diagnostic exceeds artifact budget")
	}
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	defer func() { _ = compressed.Close() }()
	defer func() { _ = writer.Close() }()
	remaining := limit - int64(len(diagnostic))
	if len(archive) > 0 {
		source, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return nil, err
		}
		defer func() { _ = source.Close() }()
		reader := tar.NewReader(io.LimitReader(source, limit+(1<<20)))
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if header.Name == name || header.Size < 0 || header.Size > remaining {
				return nil, errors.New("diagnostic artifact collision or budget")
			}
			remaining -= header.Size
			if err = writer.WriteHeader(header); err != nil {
				return nil, err
			}
			if _, err = io.CopyN(writer, reader, header.Size); err != nil {
				return nil, err
			}
		}
	}
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(diagnostic)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := writer.Write(diagnostic); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
