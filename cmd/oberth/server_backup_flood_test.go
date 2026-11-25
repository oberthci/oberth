package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// A junk-token flood on the public snapshot endpoint, from many addresses,
// must neither queue the main client that serves source delivery, admission
// and the run observer, nor reach TokenReview beyond its own small budget.
func TestBackupJunkFloodLeavesMainClientBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reviews atomic.Int32
		clients := backupFloodClients(t, &reviews)
		owner := &serverOwners{c: &composition{config: serverConfig{Namespace: "oberth"}, clock: serverClock{Now: time.Now}}, cluster: clients} //nolint:forbidigo // synctest owns this clock.
		flood := junkBackupFlood(newServerBackupSnapshot(owner), 400)
		<-time.After(500 * time.Millisecond)
		started := time.Now() //nolint:forbidigo // synctest owns this clock.
		for range 90 {
			if _, err := clients.Kube.CoreV1().Pods("oberth-runs").Get(t.Context(), "step", meta.GetOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		waited := time.Since(started) //nolint:forbidigo // synctest owns this clock.
		<-time.After(3 * time.Second)
		flood.stop()
		if waited > 0 {
			t.Fatal("backup flood queued the main cluster client", waited)
		}
		if n := reviews.Load(); n > backupReviewBurst+5 {
			t.Fatal("backup flood reached TokenReview beyond its own budget", n)
		}
		if flood.busy.Load() == 0 || flood.other.Load() == 0 {
			t.Fatal("flood was not refused fast as retryable", flood.busy.Load(), flood.other.Load())
		}
	})
}

func backupFloodClients(t *testing.T, reviews *atomic.Int32) clusterClients {
	t.Helper()
	clients, err := newClusterClients(&rest.Config{Host: "https://cluster.oberth.ci", ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}, Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var value any = &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "step", Namespace: "oberth-runs"}}
		if strings.HasSuffix(r.URL.Path, "/tokenreviews") {
			reviews.Add(1)
			value = &auth.TokenReview{}
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return clients
}

type backupFlood struct {
	busy, other atomic.Int32
	stop        func()
}

// Each address sends a junk token every 20 ms until stopped; refusals are
// counted as the fast retryable busy answer or any other refusal.
func junkBackupFlood(snapshot serverBackupSnapshot, addresses int) *backupFlood {
	f := &backupFlood{}
	ctx, cancel := context.WithCancel(context.Background())
	var junk sync.WaitGroup
	for n := range addresses {
		junk.Go(func() {
			for ctx.Err() == nil {
				_, err := snapshot.Snapshot(ctx, fmt.Sprintf("10.2.%d.%d:4000", n/256, n%256), "junk")
				if e := publicerr.Classify(err); e.Code() == publicerr.Unavailable && e.Reason() == "Backup authorization is busy." {
					f.busy.Add(1)
				} else {
					f.other.Add(1)
				}
				select {
				case <-ctx.Done():
				case <-time.After(20 * time.Millisecond):
				}
			}
		})
	}
	f.stop = func() { cancel(); junk.Wait() }
	return f
}
