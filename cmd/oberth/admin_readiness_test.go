//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdminReadinessWithoutPublicServiceEndpoints(t *testing.T) {
	directory, err := os.MkdirTemp("", "readiness-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "admin.sock")
	listener, err := listenAdminSocket(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	server := withAdminReadiness(&http.Server{Handler: http.NotFoundHandler()}, func(context.Context) error {
		return model.ReadinessFailure("ssh_identity", errors.New("private dependency details"))
	})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	var output bytes.Buffer
	if err := adminReadiness(t.Context(), socket, &output); err != nil {
		t.Fatal(err)
	}
	var state model.ReadinessResponse
	if json.Unmarshal(output.Bytes(), &state) != nil || state.Code != "ssh_identity" || state.Status != "not ready" || bytes.Contains(output.Bytes(), []byte("private dependency details")) {
		t.Fatalf("unsafe diagnostic: %s", &output)
	}
}
