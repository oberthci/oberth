package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestWiredSourceSummariesProgressWithFullNoticeChannel(t *testing.T) {
	c := &composition{health: &health{}, cacheNotices: make(chan cacheNotice, 128)}
	o := &serverOwners{c: c}
	c.hostKey = observationSigner(t)
	c.clock.Now = (clock.RealClock{}).Now
	if _, err := o.sourcePart(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := c.source
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 256 {
			r := httptest.NewRequest(http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(`{"locator":"run","descriptor":{"version":1,"kind":"main"}}`))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Errorf("unexpected status %d", w.Code)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler blocked on full diagnostic channel")
	}
	if len(c.cacheNotices) != 128 || c.health.cacheNoticeDrops.Load() != 128 || c.health.source.outcomes[0][3].Load() != 256 {
		t.Fatalf("accounting lost: notices=%d drops=%d counts=%d", len(c.cacheNotices), c.health.cacheNoticeDrops.Load(), c.health.source.outcomes[0][3].Load())
	}
	c.health.validLeaf = func() bool { return true }
	if !strings.Contains(c.health.metrics(t.Context()), "oberth_git_observation_drops_total 128\n") {
		t.Fatal("drops hidden from health")
	}
}

func TestSourceObservationDetailBound(t *testing.T) {
	text := boundedObservation(strings.Repeat("界", 2048))
	if len(text) > 2048 || !utf8.ValidString(text) || !strings.HasSuffix(text, " truncated=true") {
		t.Fatal("unbounded or invalid detail", len(text))
	}
	for text, limit := range map[string]int{sourcedelivery.CapacityReason: 40, sourcedelivery.CapacityRefusal: 80, sourcedelivery.CapacityRemedy: 120} {
		if len(text) > limit {
			t.Fatal(text)
		}
	}
}

func observationSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
