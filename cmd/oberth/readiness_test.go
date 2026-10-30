package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
)

func TestReadinessConfigurationPreservesSecretDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		secretBudget, cancelAfter, elapsed time.Duration
		want                               error
	}{
		{"shorter Secret deadline", 3 * time.Second, 0, 3 * time.Second, context.DeadlineExceeded},
		{"sampler deadline", app.ReadinessProbeBudget + time.Second, 0, app.ReadinessProbeBudget, context.DeadlineExceeded},
		{"cancellation", 20 * time.Second, time.Second, time.Second, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), app.ReadinessProbeBudget)
				defer cancel()
				if tc.cancelAfter > 0 {
					time.AfterFunc(tc.cancelAfter, cancel)
				}
				started := time.Now()
				probe := upstreamReadinessProbe(serveOptions{}, func(ctx context.Context, upstreams []model.Upstream) error {
					if len(upstreams) != 1 || upstreams[0].Name != "sampled" {
						t.Fatalf("lost upstream snapshot: %+v", upstreams)
					}
					secretCtx, cancelSecret := context.WithTimeout(ctx, tc.secretBudget)
					defer cancelSecret()
					<-secretCtx.Done()
					return fmt.Errorf("Secret lookup: %w", secretCtx.Err())
				})
				err := probe(ctx, []model.Upstream{{Name: "sampled", Kind: "ssh"}})
				if !errors.Is(err, tc.want) || time.Since(started) != tc.elapsed {
					t.Fatalf("credential result %v after %s; want %v after %s", err, time.Since(started), tc.want, tc.elapsed)
				}
				if tc.cancelAfter == 0 && tc.secretBudget < app.ReadinessProbeBudget && ctx.Err() != nil {
					t.Fatal("shorter Secret deadline unexpectedly exhausted outer budget")
				}
			})
		})
	}
}

func TestReadinessConfigurationUsesSampledDedicatedKey(t *testing.T) {
	directory := t.TempDir()
	options := serveOptions{upstreamKey: filepath.Join(directory, "missing-shared"), knownHosts: filepath.Join(directory, "known_hosts")}
	for _, path := range []string{filepath.Join(directory, "dedicated"), options.knownHosts} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	probe := upstreamReadinessProbe(options, func(context.Context, []model.Upstream) error {
		t.Fatal("sampled dedicated key should not need the Secret fallback")
		return nil
	})
	if err := probe(t.Context(), []model.Upstream{{Kind: "ssh", KeyName: "dedicated"}}); err != nil {
		t.Fatal(err)
	}
	if err := upstreamReadinessProbe(options, nil)(t.Context(), nil); err == nil {
		t.Fatal("credential check ignored the sampled upstream rows")
	}
}
