package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallationPromptKeepsApprovalInTheSameScanner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(`{"apiVersion":"v1","kind":"Config","contexts":[{"name":"chosen","context":{"cluster":"unused"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := installationSession{app: application{env: func(name string) string {
		if name != "KUBECONFIG" {
			t.Fatal("unexpected environment read", name)
		}
		return path
	}}, request: installationRequest{name: "install", flags: installationFlags("install")}}
	var out bytes.Buffer
	if err := s.prompt(t.Context(), strings.NewReader("chosen\nhttps://chosen.local\napproval-marker\n"), &out); err != nil {
		t.Fatal(err)
	}
	if s.name != "chosen" || s.choices.Context != "chosen" || s.choices.URL != "https://chosen.local" {
		t.Fatal("prompt did not select explicit installation inputs", s.choices)
	}
	if !s.input.Scan() || s.input.Text() != "approval-marker" {
		t.Fatal("prompt lost buffered approval input")
	}
}

func TestInstallationInputCancellationClosesAndJoins(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	stop := watchInstallationInput(ctx, reader)
	done := make(chan error, 1)
	go func() { _, err := reader.Read(make([]byte, 1)); done <- err }()
	cancel()
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled input stayed open")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled input did not join")
	}
}
