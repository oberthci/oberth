package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAuthReadersRejectBlankStoredBinding(t *testing.T) {
	for _, field := range []string{"identity", "fingerprint"} {
		for _, blank := range []string{"", " \t "} {
			t.Run(fmt.Sprintf("%s/%q", field, blank), func(t *testing.T) {
				s, uplink := authReaderFixture(t)
				// The schema permits these values, even though normal mutation
				// paths reject them. Auth readers must fail closed on bad data.
				if _, err := s.db.ExecContext(t.Context(), "UPDATE uplinks SET "+field+"=? WHERE id=?", blank, uplink.ID); err != nil {
					t.Fatal(err)
				}
				fingerprint := uplink.Fingerprint
				if field == "fingerprint" {
					fingerprint = blank
				}
				t.Run("bearer", func(t *testing.T) {
					got, err := s.AuthenticatedUplinkByDigest(t.Context(), make([]byte, 32))
					if !errors.Is(err, ErrInvalidState) || got.ID != 0 || got.Identity != "" {
						t.Fatalf("malformed binding authenticated: %+v err=%v", got, err)
					}
				})
				t.Run("ssh", func(t *testing.T) {
					got, err := s.UplinkByFingerprint(t.Context(), fingerprint)
					if !errors.Is(err, ErrInvalidState) || got.ID != 0 || got.Identity != "" {
						t.Fatalf("malformed binding authenticated: %+v err=%v", got, err)
					}
				})
			})
		}
	}
}

func TestBearerIdentityExpiredAdmissionReturnsError(t *testing.T) {
	s, _ := authReaderFixture(t)
	for _, kind := range []string{"canceled", "expired"} {
		t.Run(kind, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if kind == "expired" {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
			}
			defer cancel()
			got, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
			if !errors.Is(err, want) || got.ID != 0 || got.Identity != "" || got.Scope != nil {
				t.Fatalf("expired admission returned unexpected value/error: %+v err=%v want=%v", got, err, want)
			}
			t.Logf("expired admission: id=%d identity=%q err=%v", got.ID, got.Identity, err)
		})
	}
}
