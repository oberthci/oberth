package gitcache

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestBusyExportWaitIsCancelableBeforeAnyGitOrRepositoryLock(t *testing.T) {
	t.Parallel()
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: "/uncontacted/git"})
	if err != nil {
		t.Fatal(err)
	}
	b := sourcecontract.Binding{
		Template:     "build",
		Artifact:     "source",
		Path:         "/work/src",
		RepositoryID: 1,
		Revision: sourcecontract.Revision{
			Repository: "origin/team/widget",
			Commit:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main},
	}
	c.export <- struct{}{}
	c.repositoryGate("blocked") <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Stage(ctx, b)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy export touched Git: %v", err)
	}
	if len(c.export) != 1 || len(c.repositoryGate("blocked")) != 1 {
		t.Fatal("busy export changed slot ownership")
	}
	<-c.export
	<-c.repositoryGate("blocked")
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestAuxiliaryByteGuardsAcceptNRefuseNPlusOne(t *testing.T) {
	t.Parallel()
	for _, limit := range []int64{PackBytes, MetadataBytes, MainArchiveBytes, PinArchiveBytes} {
		writer := &limitedWriter{writer: io.Discard, left: limit}
		if n, err := io.CopyN(writer, zeroReader{}, limit); err != nil || n != limit {
			t.Fatalf("N %d: %d %v", limit, n, err)
		}
		if _, err := writer.Write([]byte{0}); err == nil {
			t.Fatalf("N+1 %d accepted", limit)
		}
	}
}
