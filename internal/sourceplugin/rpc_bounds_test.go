package sourceplugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestNativeRPCMessageAccepts4MiBRefusesOneMoreByte(t *testing.T) {
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "rpc-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "source.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, &Client{}) }()
	waitForSocket(t, path)
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket permissions", err)
	}
	connection, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	client := artifact.NewArtifactServiceClient(connection)
	for _, size := range []int{4 << 20, (4 << 20) + 1} {
		checkRPCSize(t, client, size)
	}
}
func checkRPCSize(t *testing.T, client artifact.ArtifactServiceClient, size int) {
	t.Helper()

	request := &artifact.IsDirectoryRequest{Artifact: &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run", Configuration: `{"version":1,"kind":"main"}`}}}
	length := size - request.Size() - 10
	for {
		request.Artifact.Name = strings.Repeat("x", length)
		delta := size - request.Size()
		if delta == 0 {
			break
		}
		length += delta
	}
	response, err := client.IsDirectory(t.Context(), request, grpc.MaxCallSendMsgSize(size), grpc.WaitForReady(true))
	if size == 4<<20 {
		if err != nil || response.IsDirectory {
			t.Fatal("N refused or directory response", err)
		}
	} else if status.Code(err) != codes.ResourceExhausted {
		t.Fatal("N+1 not refused", err)
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.After(time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatal("plugin socket not created")
		}
	}
}
