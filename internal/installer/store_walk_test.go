package installer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestFreshInstallReadsRecordedCAThroughStorePhase(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(boolText(missing), func(t *testing.T) { walkStoreCA(t, missing) })
	}
}
func walkStoreCA(t *testing.T, missing bool) {
	t.Helper()
	f, backend := storeEngine(t)
	p := prepare(t, f)
	reached := false
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if phase != installer.StartingStore {
			return nil
		}
		reached = true
		return checkStoreCAFile(t, f.dir, missing)
	})
	if !reached {
		t.Fatal("install did not reach store", err)
	}
	if missing {
		var p *installer.Problem
		if !errors.As(err, &p) || p.Code != "credential-missing" || !strings.Contains(p.Reason, "missing") || len(backend.Calls) != 0 {
			t.Fatal("missing CA was not refused before opening store", err)
		}
		return
	}
	if err != nil || !backend.Revoked {
		t.Fatal("fresh install failed store step", err)
	}
}

// Raft's unseal response can precede leader readiness. A configuration read
// during that gap fails; the install must wait on health without replaying writes.
type activatingStore struct {
	backend    storefake.Store
	health     []int
	requests   []string
	refusePath string
}

func (s *activatingStore) RoundTrip(r *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	s.requests = append(s.requests, path)
	if path == "/sys/health" && len(s.health) > 0 {
		code := s.health[0]
		s.health = s.health[1:]
		return storeHTTP(r, code), nil
	}
	if path == s.refusePath {
		return storeHTTP(r, http.StatusForbidden), nil
	}
	if path == "/sys/mounts" && len(s.health) > 0 {
		return storeHTTP(r, http.StatusInternalServerError), nil
	}
	return s.backend.RoundTrip(r)
}
func storeHTTP(r *http.Request, code int) *http.Response {
	_ = r.Body.Close()
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}
}
func activatingFixture(t *testing.T, backend *activatingStore) fixture {
	t.Helper()
	f := setup(t)
	f.engine.Pause = func(context.Context) error { return nil }
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
		return c, func() {}, err
	}
	return f
}
func TestStoreWaitsForActiveOnFreshAndResumedBootstrap(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(boolText(resume), func(t *testing.T) {
			walkActivation(t, resume)
		})
	}
}
func TestStoreActivationDoesNotRetryPermanentFailures(t *testing.T) {
	for _, path := range []string{"/sys/health", "/sys/mounts"} {
		t.Run(path, func(t *testing.T) {
			backend := &activatingStore{refusePath: path}
			f := activatingFixture(t, backend)
			p := prepare(t, f)
			if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
				t.Fatal("authentication refusal hidden")
			}
			if countPath(backend.requests, path) != 1 || backend.backend.Revoked {
				t.Fatal("permanent refusal retried", backend.requests)
			}
		})
	}
}
func TestStoreActivationUsesExistingPhaseDeadline(t *testing.T) {
	backend := &activatingStore{health: []int{429, 429}}
	f := activatingFixture(t, backend)
	f.engine.Pause = func(context.Context) error { return context.DeadlineExceeded }
	p := prepare(t, f)
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "store-timeout" || countPath(backend.requests, "/sys/mounts") != 0 {
		t.Fatal("activation escaped phase deadline", err)
	}
}
func countPath(paths []string, expected string) int {
	count := 0
	for _, path := range paths {
		if path == expected {
			count++
		}
	}
	return count
}

func readStoreProfile(t *testing.T, dir string) installer.Profile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p installer.Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func checkStoreCAFile(t *testing.T, dir string, missing bool) error {
	t.Helper()
	profile := readStoreProfile(t, dir)
	if !strings.HasPrefix(filepath.Base(profile.CAFile), "ca-") {
		t.Fatal("CA name lost content binding")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); !os.IsNotExist(err) {
		t.Fatal("legacy CA exists", err)
	}
	if missing {
		return os.Remove(profile.CAFile)
	}
	return nil
}

func walkActivation(t *testing.T, resume bool) {
	t.Helper()
	backend := &activatingStore{health: []int{503, 429, 200}}
	f := activatingFixture(t, backend)
	if resume {
		f.engine.Pause = func(context.Context) error { return context.Canceled }
		p := prepare(t, f)
		if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); !errors.Is(err, context.Canceled) {
			t.Fatal("did not interrupt activation", err)
		}
		f.engine.Pause = func(context.Context) error { return nil }
	}
	apply(t, f, prepare(t, f))
	if !backend.backend.Revoked || len(backend.health) != 0 {
		t.Fatal("did not wait until active and finish")
	}
	for _, path := range []string{"/sys/init", "/sys/unseal", "/sys/mounts/oberth", "/auth/token/revoke-self"} {
		if countPath(backend.requests, path) != 1 {
			t.Fatal("bootstrap repeated", path, backend.requests)
		}
	}
	if countPath(backend.requests, "/sys/health") != 3 {
		t.Fatal("health was not polled", backend.requests)
	}
}

func TestStoreUnexpectedHealthNamesStatus(t *testing.T) {
	for _, status := range []int{204, 403, 500} {
		backend := &activatingStore{health: []int{status}}
		f := activatingFixture(t, backend)
		p := prepare(t, f)
		_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "store-health" || !strings.Contains(refusal.Detail, fmt.Sprintf("HTTP %d", status)) {
			t.Fatal("unexpected health lost its cause", err)
		}
		if len(refusal.Reason) > 40 || len(refusal.Remedy) > 120 || countPath(backend.requests, "/sys/health") != 1 || countPath(backend.requests, "/sys/mounts") != 0 {
			t.Fatal("health failure was retried or misreported", refusal, backend.requests)
		}
	}
}
