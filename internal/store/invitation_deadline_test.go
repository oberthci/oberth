package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestInvitationReservationCannotOutliveInvitation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	invite, input := atomicEnrollment(t, s)
	requireOK(t, s.Invite(t.Context(), stateport.Action{Actor: "operator"}, invite))
	_, before, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	changed := observeChanges(t, s)
	reservation := stateport.InvitationReservation{
		ID: input.InvitationID, Digest: input.Digest, Token: "too-long",
		Until: invite.Public.ExpiresAt.Add(time.Nanosecond),
	}
	_, err = s.ReserveInvitation(t.Context(), reservation)
	want := "forbidden: invitation expired or its requested scope differs - Obtain a fresh invitation with the exact intended identity and scope."
	if err == nil || err.Error() != want {
		t.Fatalf("reservation beyond invitation expiry: %v; want %s", err, want)
	}
	_, after, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	if after != before {
		t.Fatal("refused reservation changed the audit head")
	}
	select {
	case <-changed:
		t.Fatal("refused reservation notified a change")
	default:
	}
	reservation.Token = input.Reservation
	reservation.Until = invite.Public.ExpiresAt
	got, err := s.ReserveInvitation(t.Context(), reservation)
	requireOK(t, err)
	if !reflect.DeepEqual(got, invite.Public) {
		t.Fatalf("reservation changed invitation: %#v", got)
	}
	requireOK(t, s.Enroll(t.Context(), input))
	key, err := s.Key(t.Context(), input.Key.Fingerprint)
	requireOK(t, err)
	if key.Identity != invite.Public.Identity || !reflect.DeepEqual(key.Scope, invite.Public.Scope) {
		t.Fatalf("enrollment did not preserve invited identity and scope: %#v", key)
	}
}
