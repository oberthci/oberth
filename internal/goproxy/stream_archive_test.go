package goproxy

import (
	"archive/tar"
	"context"
	"io"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
)

type streamedArchiveCache struct {
	mockCache
	finished atomic.Bool
}

func (c *streamedArchiveCache) ArchiveTar(ctx context.Context, _, _ string, w io.Writer) error {
	defer c.finished.Store(true)
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()
	const size = 64 << 20
	if err := tw.WriteHeader(&tar.Header{Name: "source.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: size}); err != nil {
		return err
	}
	chunk := make([]byte, 32<<10)
	for n := 0; n < size; n += len(chunk) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tw.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func TestOversizedModuleArchiveHasBoundedHeapAndJoinsProducer(t *testing.T) {
	cache := &streamedArchiveCache{mockCache: *mockModuleCache([]string{"v1.0.0"}, "go.example.test/wire")}
	h := cachedTestHandler(t, cache, "go.example.test", map[string]string{"wire": "repo"})
	h.archiveLimit = 16 << 20
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest("GET", "/go.example.test/wire/@v/v1.0.0.zip", nil))
	runtime.ReadMemStats(&after)
	if response.Code < 400 || response.Header().Get("X-Oberth-GoProxy-Error") != "archive_too_large" {
		t.Errorf("oversized tar accepted: %d", response.Code)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Errorf("archive buffering allocated %d bytes for a 16 MiB cap", allocated)
	}
	if !cache.finished.Load() {
		t.Error("archive producer outlived rejected build")
	}
}
