package gitcache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestRepositoryAndRecoveryWaitsRevokeBeforeAnySourceBytes(t *testing.T) {
	for _, path := range []string{"miss", "hit", "recovery-miss", "recovery-hit", "join", "room"} {
		for _, change := range []string{"pod", "terminal", "digest"} {
			t.Run(path+"/"+change, func(t *testing.T) { synctest.Test(t, func(t *testing.T) { checkSourceWaitAuthority(t, path, change) }) })
		}
	}
}

type sourceWaitCase struct {
	summary sourcedelivery.Summary
	exportWaitFixture
	path                     string
	changed                  atomic.Bool
	authorizations, receipts atomic.Int32
	held                     []sourcedelivery.Staged
	peer                     chan exportWaitResult
	unlock                   func()
}

func checkSourceWaitAuthority(t *testing.T, path, change string) {
	f := &sourceWaitCase{exportWaitFixture: newExportWaitFixture(t), path: path, peer: make(chan exportWaitResult, 1), unlock: func() {}}
	f.prepare(t)
	defer f.cleanup()
	server := sourcedelivery.New(f.config(change))
	data, err := json.Marshal(sourcedelivery.LoadRequest{Locator: "run", Descriptor: f.binding.Descriptor})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(string(data)))
	req.Header.Set("Authorization", "Bearer bound")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { server.ServeHTTP(w, req); close(done) }()
	if !strings.HasPrefix(path, "recovery") {
		synctest.Wait()
		if f.authorizations.Load() != 1 {
			t.Errorf("load did not block after initial authorization: %d", f.authorizations.Load())
		}
		f.changed.Store(true)
		if path == "room" {
			_ = f.held[0].Body.Close()
		} else {
			f.unlock()
		}
	}
	<-done
	if path == "join" {
		peer := <-f.peer
		if peer.err != nil {
			t.Fatal(peer.err)
		}
		_ = peer.stage.Body.Close()
	}
	f.checkResponse(t, w)
}

func (f *sourceWaitCase) prepare(t *testing.T) {
	if strings.Contains(f.path, "hit") {
		s, err := f.cache.Stage(t.Context(), f.binding)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Body.Close()
	}
	if f.path == "room" {
		for i := range 2 {
			b := f.binding
			b.Template += strings.Repeat("x", i+1)
			s, err := f.cache.Stage(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			f.held = append(f.held, s)
		}
		return
	}
	if strings.HasPrefix(f.path, "recovery") {
		if err := os.WriteFile(filepath.Join(f.exportWaitFixture.path, reconciliationFile), []byte("refs/heads/main\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := f.cache.lock(t.Context(), f.exportWaitFixture.path); err != nil {
		t.Fatal(err)
	}
	f.unlock = sync.OnceFunc(func() { f.cache.unlock(f.exportWaitFixture.path) })
	if f.path == "join" {
		go func() { s, err := f.cache.Stage(t.Context(), f.binding); f.peer <- exportWaitResult{s, err} }()
		synctest.Wait()
	}
}

func (f *sourceWaitCase) config(change string) sourcedelivery.Config {
	return sourcedelivery.Config{Metric: func(s sourcedelivery.Summary) { f.summary = s }, Now: f.cache.config.Now, Stage: func(ctx context.Context, b sourcecontract.Binding) (sourcedelivery.Staged, error) {
		if strings.HasPrefix(f.path, "recovery") {
			f.changed.Store(true)
		}
		return f.cache.Stage(ctx, b)
	}, Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
		f.authorizations.Add(1)
		if change == "pod" && f.changed.Load() {
			return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "forbidden"}
		}
		return sourcedelivery.Caller{Namespace: "runs", Workflow: "workflow", UID: "workflow-uid"}, nil
	}, Run: func(context.Context, string) (sourcedelivery.Run, error) {
		digest := strings.Repeat("a", 64)
		if change == "digest" && f.changed.Load() {
			digest = strings.Repeat("b", 64)
		}
		return sourcedelivery.Run{Namespace: "runs", Workflow: "workflow", UID: "workflow-uid", Terminal: change == "terminal" && f.changed.Load(), Admission: admissioncontract.PreparedView{RunID: "run", Digest: digest, SourceBindings: []sourcecontract.Binding{f.binding}}}, nil
	}, RecordSuccess: func(context.Context, sourcedelivery.Caller, time.Time) error { f.receipts.Add(1); return nil }}
}

func (f *sourceWaitCase) checkResponse(t *testing.T, w *httptest.ResponseRecorder) {
	if w.Code != http.StatusForbidden || w.Body.String() != "{\"version\":1,\"code\":\"forbidden\"}\n" {
		t.Errorf("stale authority sent source: status=%d body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get(sourcedelivery.ReceiptTrailer) != "" || f.receipts.Load() != 0 {
		t.Error("stale authority got a completion receipt")
	}
	if f.authorizations.Load() != 2 {
		t.Error("wait did not trigger exactly one live recheck", f.authorizations.Load())
	}
	f.checkPhase(t)
	if len(f.cache.export) != 0 {
		t.Error("export permit leaked")
	}
	f.cache.staged.mu.Lock()
	defer f.cache.staged.mu.Unlock()
	for _, e := range f.cache.staged.entries {
		if e.readers != 0 && f.path != "room" {
			t.Error("wait leaked an archive reader")
		}
		if e.waiters != 0 || e.building {
			t.Error("wait leaked archive ownership")
		}
	}
}

func (f *sourceWaitCase) cleanup() {
	f.unlock()
	for _, s := range f.held {
		_ = s.Body.Close()
	}
}

func (f *sourceWaitCase) checkPhase(t *testing.T) {
	want := sourcedelivery.Repository
	if f.path == "room" {
		want = sourcedelivery.Archive
	}
	if strings.HasPrefix(f.path, "recovery") {
		want = sourcedelivery.Recovery
	}
	if !f.summary.Blocked || f.summary.Phase != want {
		t.Error("last blocking phase was lost", f.summary.Phase, want)
	}
}
