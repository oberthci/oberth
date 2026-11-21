package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourceplugin"
)

func TestPinnedHTTPSHeaderAccepts64KiBRefusesOneMoreByte(t *testing.T) {
	for _, size := range []int{64 << 10, (64 << 10) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cfg := pluginConfig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				prefix := "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nX-Pad: "
				_, _ = buffer.WriteString(prefix + strings.Repeat("x", size-len(prefix)-4) + "\r\n\r\n")
				_ = buffer.Flush()
				_ = sshtest.Flush(conn.(*tls.Conn).NetConn())
			}))
			cfg.Jitter = func(time.Duration) time.Duration { return time.Nanosecond }
			cfg.Wait = func(context.Context, time.Duration) error {
				t.Error("terminal header response was retried")
				return context.Canceled
			}
			client, err := sourceplugin.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			err = client.Load(t.Context(), &artifact.Artifact{Plugin: &artifact.PluginArtifact{Name: "oberth", Key: "run", Configuration: `{"version":1,"kind":"main"}`}}, filepath.Join(t.TempDir(), "archive"))
			if err == nil {
				t.Fatal("403 accepted")
			}
			denied := strings.Contains(err.Error(), "source access is forbidden")
			if denied != (size == 64<<10) {
				t.Fatalf("header %d: %v", size, err)
			}
		})
	}
}
