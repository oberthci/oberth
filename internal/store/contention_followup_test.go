package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestTokenIdentityAndScopesUseOneSnapshot(t *testing.T) {
	s, uplink := authReaderFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(10*time.Second))
	defer cancel()
	update := func(state string) error {
		_, err := s.UpsertUplink(ctx, model.UplinkSpec{
			Fingerprint: uplink.Fingerprint, Identity: uplink.Identity,
			TokenCredentialID: uplink.TokenCredentialID, AuthActor: state, Scope: []string{state},
		})
		return err
	}
	if err := update("first"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 5)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		for i := range 256 {
			if err := update([]string{"first", "second"}[i%2]); err != nil {
				errs <- err
				return
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			<-start
			for range 256 {
				got, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
				if err != nil {
					errs <- err
					return
				}
				if len(got.Scope) != 1 || got.Scope[0] != got.AuthActor {
					errs <- fmt.Errorf("mixed token snapshot: actor=%q scopes=%v", got.AuthActor, got.Scope)
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestAuditScansDoNotConsumeMappingReaders(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	createRepo(t, s)
	var release []func()
	defer func() {
		for _, close := range release {
			close()
		}
	}()
	for range auditReadConnections {
		conn, err := s.auditReadDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		release = append(release, func() { _ = conn.Close() })
		if _, err := conn.ExecContext(t.Context(), `DELETE FROM runs`); err == nil {
			t.Fatal("audit reader accepted a write")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.auditHashIndex(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted audit pool: %v", err)
	}
	lookup, stop := context.WithTimeout(t.Context(), testtimeout.Scale(time.Second))
	defer stop()
	if _, err := s.RepositoryByName(lookup, "codeberg/codeberg.org/oberth"); err != nil {
		t.Fatalf("mapping blocked by audit readers: %v", err)
	}
	if _, err := s.db.ExecContext(lookup, `UPDATE repositories SET updated_at=updated_at`); err != nil {
		t.Fatalf("writer blocked by audit readers: %v", err)
	}
}

func TestCloseDoesNotHoldMutexDuringCleanup(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	private, err := InspectCurrent(t.Context(), s.livePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	cleanup := private.cleanup
	private.cleanup = func() error {
		close(entered)
		<-release
		return cleanup()
	}
	closed := make(chan error, 1)
	go func() { closed <- private.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Scale(time.Second))
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("close did not reach cleanup")
	}
	if !private.mu.TryLock() {
		t.Fatal("close holds Store mutex while cleanup is blocked")
	}
	private.mu.Unlock()
	if err := private.Close(); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("close did not finish after cleanup was released")
	}
}
