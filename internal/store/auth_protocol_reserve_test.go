package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

// Model three descheduled in-flight lookups: each owns its protocol admission
// and a real database/sql connection until the test releases it.
func holdProtocolAuthReads(t *testing.T, s *Store, slots chan struct{}) func() {
	t.Helper()
	var held []*sql.Conn
	var releases []func()
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, conn := range held {
				_ = conn.Close()
			}
			for _, done := range releases {
				done()
			}
		})
	}
	t.Cleanup(release)
	for range authProtocolSlots {
		done, err := acquireAuthSlot(t.Context(), slots)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, done)
		conn, err := s.authDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	return release
}

func bearerReadOperations(s *Store, uplink model.Uplink) []func(context.Context) error {
	return []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
			return err
		},
		func(ctx context.Context) error {
			_, err := s.TokenCredentialByDigest(ctx, make([]byte, 32))
			return err
		},
		func(ctx context.Context) error { return s.TouchTokenCredential(ctx, uplink.TokenCredentialID) },
	}
}

func TestAuthProtocolReservePreventsConcurrentCrossStarvation(t *testing.T) {
	for _, floodProtocol := range []string{"bearer", "ssh"} {
		t.Run(floodProtocol+"-flood", func(t *testing.T) {
			s, uplink := authReaderFixture(t)
			// The touch operation must reach its no-op active-token read.
			if err := s.TouchTokenCredential(t.Context(), uplink.TokenCredentialID); err != nil {
				t.Fatal(err)
			}
			ssh := func(ctx context.Context) error { _, err := s.UplinkByFingerprint(ctx, uplink.Fingerprint); return err }
			unknownBearer := func(ctx context.Context) error {
				digest := make([]byte, 32)
				digest[0] = 1
				_, err := s.AuthenticatedUplinkByDigest(ctx, digest)
				if errors.Is(err, ErrNotFound) {
					return nil
				}
				return err
			}
			unknownSSH := func(ctx context.Context) error {
				_, err := s.UplinkByFingerprint(ctx, "SHA256:unknown-key")
				if errors.Is(err, ErrNotFound) {
					return nil
				}
				return err
			}
			bearer := bearerReadOperations(s, uplink)
			slots, flood, peer := s.bearerAuthSlots, append([]func(context.Context) error{unknownBearer}, bearer...), []func(context.Context) error{ssh}
			if floodProtocol == "ssh" {
				slots, flood, peer = s.sshAuthSlots, []func(context.Context) error{unknownSSH, ssh}, bearer
			}
			release := holdProtocolAuthReads(t, s, slots)
			floodCtx, cancelFlood := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			defer func() { cancelFlood(); wg.Wait(); release() }()
			ready, start := make(chan struct{}, 64), make(chan struct{})
			results := make(chan error, 64)
			var completed atomic.Int64
			for i := range 64 {
				wg.Go(func() {
					ready <- struct{}{}
					<-start
					for {
						err := flood[i%len(flood)](floodCtx)
						if err != nil {
							results <- err
							return
						}
						completed.Add(1)
					}
				})
			}
			for range 64 {
				<-ready
			}
			close(start)
			// More same-protocol work expires at its own admission queue. It cannot
			// occupy the fourth connection that the opposite protocol needs.
			queued, cancelQueued := context.WithTimeout(t.Context(), 20*time.Millisecond)
			err := flood[0](queued)
			cancelQueued()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("saturated %s admission ignored deadline: %v", floodProtocol, err)
			}
			for i := range 24 {
				ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(500*time.Millisecond))
				err := peer[i%len(peer)](ctx)
				cancel()
				if err != nil {
					t.Fatalf("%s flood starved opposite protocol at lookup%d: %v", floodProtocol, i, err)
				}
			}
			if completed.Load() != 0 {
				t.Fatalf("%s escaped its three owned slots: %d lookups", floodProtocol, completed.Load())
			}
			cancelFlood()
			wg.Wait()
			for range 64 {
				if err := <-results; !errors.Is(err, context.Canceled) {
					t.Fatalf("flood waiter did not cancel cleanly: %v", err)
				}
			}
			release()
			for _, lookup := range flood {
				if err := lookup(t.Context()); err != nil {
					t.Fatalf("protocol slot leaked: %v", err)
				}
			}
		})
	}
}

func TestAuthProtocolAdmissionIsPerStoreAndAlreadyCanceled(t *testing.T) {
	first, one := authReaderFixture(t)
	second, two := authReaderFixture(t)
	release := holdProtocolAuthReads(t, first, first.sshAuthSlots)
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(500*time.Millisecond))
	defer cancel()
	if _, err := second.UplinkByFingerprint(ctx, two.Fingerprint); err != nil {
		t.Fatalf("another Store shared protocol slots: %v", err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if _, err := first.AuthenticatedUplinkByDigest(canceled, make([]byte, 32)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bearer acquired a free slot: %v", err)
	}
	if _, err := first.UplinkByFingerprint(canceled, one.Fingerprint); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled SSH waited: %v", err)
	}
}
