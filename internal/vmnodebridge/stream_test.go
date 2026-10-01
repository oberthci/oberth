package vmnodebridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("test source failure") }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("test sink failure") }

func TestStreamCopyFailureCancelsIgnoredRemoteCommandErrors(t *testing.T) {
	for _, mode := range []string{"input-error", "input-overflow", "output-error", "output-overflow"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "input-error":
				reader := &boundedReader{source: failingReader{}, left: 3, cancel: cancel}
				if _, err := reader.Read(make([]byte, 3)); !errors.Is(err, errRuntimeStream) {
					t.Fatal("input copy error was not bounded")
				}
			case "input-overflow":
				reader := &boundedReader{source: bytes.NewReader([]byte("abcd")), left: 3, cancel: cancel}
				_, _ = io.Copy(io.Discard, reader)
			case "output-error":
				writer := &boundedWriter{target: failingWriter{}, left: 3, cancel: cancel}
				if _, err := writer.Write([]byte("abc")); !errors.Is(err, errRuntimeStream) {
					t.Fatal("output copy error was not bounded")
				}
			case "output-overflow":
				writer := &boundedWriter{target: io.Discard, left: 3, cancel: cancel}
				if _, err := writer.Write([]byte("abcd")); !errors.Is(err, errRuntimeStream) {
					t.Fatal("output overflow was not refused")
				}
			}
			if ctx.Err() == nil {
				t.Fatal("remote-command stream not canceled after hidden copy failure")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &boundedReader{source: bytes.NewReader([]byte("abc")), left: 3, cancel: cancel}
	if _, err := io.Copy(io.Discard, reader); err != nil || ctx.Err() != nil {
		t.Fatalf("exact input bound falsely canceled: %v", err)
	}
}
