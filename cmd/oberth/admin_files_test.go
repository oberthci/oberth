package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminReadFileAndDatabaseHashes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	database := filepath.Join(directory, "oberth.sqlite")
	main := []byte("database\x00binary\n")
	wal := []byte("WAL\n")
	for path, body := range map[string][]byte{database: main, database + "-wal": wal} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := runAdmin(t.Context(), []string{"read-file", filepath.Join(directory, "missing"), database}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), main) {
		t.Fatal("file bytes were changed")
	}
	output.Reset()
	if err := runAdmin(t.Context(), []string{"hash-db", "--database", database}, &output); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x  %s\n%x  %s-wal\n", sha256.Sum256(main), database, sha256.Sum256(wal), database)
	if output.String() != want {
		t.Fatalf("hash output=%q want %q", output.String(), want)
	}
	if err := os.Remove(database + "-wal"); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runAdmin(t.Context(), []string{"hash-db", "--database", database}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatal("absent WAL was not optional")
	}
	for _, args := range [][]string{{"read-file", directory}, {"read-file", "relative"}, {"read-file", filepath.Join(directory, "missing")}, {"hash-db", "--database", filepath.Join(directory, "missing")}} {
		if err := runAdmin(t.Context(), args, io.Discard); err == nil {
			t.Fatalf("invalid file invocation accepted: %v", args)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runAdmin(ctx, []string{"read-file", database}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	sentinel := errors.New("output closed")
	if err := runAdmin(t.Context(), []string{"read-file", database}, adminFailWriter{sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("copy write error lost: %v", err)
	}
	if err := runAdmin(t.Context(), []string{"hash-db", "--database", database}, adminFailWriter{sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("hash write error lost: %v", err)
	}
}

type adminFailWriter struct{ err error }

func (w adminFailWriter) Write([]byte) (int, error) { return 0, w.err }
