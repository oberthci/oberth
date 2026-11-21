package main

import (
	"context"
	"fmt"
	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/sourceplugin"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSecondInterruptedTLS(t *testing.T) {
	for _, mode := range []string{"truncated", "disconnect", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := pluginConfig(t, interruptedTransfer(mode, cancel))
			client, e := sourceplugin.New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer client.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "source.tgz")
			if e = os.WriteFile(path, []byte("existing"), 0600); e != nil {
				t.Fatal(e)
			}
			e = client.Load(ctx, &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: `{"version":1,"kind":"main"}`}}, path)
			if e == nil {
				t.Error("interrupted response accepted")
			}
			data, _ := os.ReadFile(path)
			entries, _ := os.ReadDir(dir)
			if string(data) != "existing" || len(entries) != 1 {
				t.Errorf("destination or temp residue: %q %d", data, len(entries))
			}
			t.Log("refused and cleaned:", e)
		})
	}
}

func interruptedTransfer(mode string, cancel context.CancelFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", sourcedelivery.LoadContentType)
		w.Header().Set("Trailer", sourcedelivery.ReceiptTrailer)
		_, _ = fmt.Fprint(w, "partial gzip")
		_ = http.NewResponseController(w).Flush()
		if mode == "disconnect" {
			panic(http.ErrAbortHandler)
		}
		if mode == "cancel" {
			cancel()
			<-r.Context().Done()
		}
	})
}
