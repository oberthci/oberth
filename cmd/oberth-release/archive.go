package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// archive normalizes ordering, modes, ownership and timestamps.
func archive(files map[string][]byte, executable bool) ([]byte, error) {
	var body bytes.Buffer
	tw := tar.NewWriter(&body)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		mode := int64(0644)
		if executable {
			mode = 0555
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}
func readArchive(r io.Reader) (map[string][]byte, error) {
	files := make(map[string][]byte)
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if _, exists := files[header.Name]; exists {
			return nil, inputs
		}
		files[header.Name], err = io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
	}
}
func writeFile(o options, name string, body []byte) error {
	return os.WriteFile(filepath.Join(o.dir, name), body, 0644)
}
func compressed(body []byte) ([]byte, error) {
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(body); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
