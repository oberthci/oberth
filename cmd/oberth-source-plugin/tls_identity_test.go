package main

import (
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourceplugin"
)

func TestBearerNeedsPinnedCAAndServerName(t *testing.T) {
	for _, kind := range []string{"trusted", "other CA", "other name"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			s := &sourceIdentity{}
			delivery := s.handler(t)
			cfg := pluginConfig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				delivery.ServeHTTP(w, r)
			}))
			switch kind {
			case "other CA":
				_, cfg.CAPEM = testCertificate(t)
			case "other name":
				cfg.ServerName = "other.test"
			}
			client, err := sourceplugin.New(cfg)
			storeOK(t, err)
			defer client.Close()
			input := &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run-1", Configuration: `{"version":1,"kind":"main"}`}}
			path := filepath.Join(t.TempDir(), "source.tgz")
			err = client.Load(t.Context(), input, path)
			if kind == "trusted" {
				storeOK(t, err)
				checkSourceArchive(t, path)
				if requests.Load() != 1 {
					t.Fatal("trusted server did not receive exactly one request")
				}
				return
			}
			if err == nil || requests.Load() != 0 {
				t.Fatal("bearer reached a server without the pinned CA and name")
			}
		})
	}
}
