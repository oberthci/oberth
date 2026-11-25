package store

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func alteredArchive(t *testing.T, body []byte, alter func(*tar.Header, []byte) []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	reader := tar.NewReader(bytes.NewReader(body))
	writer := tar.NewWriter(&output)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		requireOK(t, err)
		data, err := io.ReadAll(reader)
		requireOK(t, err)
		data = alter(header, data)
		header.Size = int64(len(data))
		requireOK(t, writer.WriteHeader(header))
		_, err = writer.Write(data)
		requireOK(t, err)
	}
	requireOK(t, writer.Close())
	return output.Bytes()
}

func TestArchiveModeReportAndManifestBounds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var original bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &original)
	requireOK(t, err)
	for _, mode := range []string{"mode", "report", "manifest-limit", "manifest-limit-plus-one"} {
		t.Run(mode, func(t *testing.T) {
			modified := alteredArchive(t, original.Bytes(), archiveAlteration(t, mode))
			_, err := backupForTest(t, s).Inspect(t.Context(), backupOptions(s), bytes.NewReader(modified))
			if mode == "manifest-limit" {
				requireOK(t, err)
			} else {
				requireCode(t, err, publicerr.SchemaRefused)
			}
		})
	}
}

func TestArchiveDatabaseSizeAtLimitAndOneByteBelow(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var original bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &original)
	requireOK(t, err)
	reader := tar.NewReader(bytes.NewReader(original.Bytes()))
	header, err := reader.Next()
	requireOK(t, err)
	options := backupOptions(s)
	options.MaxBytes = header.Size
	_, err = backupForTest(t, s).Inspect(t.Context(), options, bytes.NewReader(original.Bytes()))
	requireOK(t, err)
	options.MaxBytes--
	_, err = backupForTest(t, s).Inspect(t.Context(), options, bytes.NewReader(original.Bytes()))
	requireCode(t, err, publicerr.InvalidInput)
}

func archiveAlteration(t *testing.T, mode string) func(*tar.Header, []byte) []byte {
	t.Helper()
	return func(h *tar.Header, data []byte) []byte {
		if mode == "mode" {
			h.Mode = 0644
		}
		if h.Name != "manifest.json" {
			return data
		}
		if mode == "report" {
			var m manifest
			requireOK(t, json.Unmarshal(data, &m))
			m.Integrity = false
			return []byte(encode(m))
		}
		if mode == "manifest-limit" || mode == "manifest-limit-plus-one" {
			size := 8192
			if mode == "manifest-limit-plus-one" {
				size++
			}
			return append(data, bytes.Repeat([]byte(" "), size-len(data))...)
		}
		return data
	}
}

func TestArchiveByteLimitIncludesPartialSQLitePages(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "padded.sqlite")
	requireOK(t, snapshotDatabase(t.Context(), s.path, path, stateport.DefaultMaxBytes))
	info, err := os.Stat(path)
	requireOK(t, err)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	requireOK(t, err)
	_, err = file.Write([]byte{0})
	requireOK(t, err)
	requireOK(t, file.Close())
	var archive bytes.Buffer
	report := stateport.BackupReport{Schema: Schema, AuditHead: independentHead(t, s.readers), Integrity: true, ForeignKeys: true}
	options := backupOptions(s)
	requireOK(t, emitArchive(t.Context(), archiveOutput{path, options, report, &archive, nil}))
	options.MaxBytes = info.Size()
	_, err = backupForTest(t, s).Inspect(t.Context(), options, bytes.NewReader(archive.Bytes()))
	requireCode(t, err, publicerr.InvalidInput)
}
