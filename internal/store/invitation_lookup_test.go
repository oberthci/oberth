package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestInvitationLookupDoesNotReserveAndIsSingleUse(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	invite, input := atomicEnrollment(t, s)
	requireOK(t, s.Invite(t.Context(), stateport.Action{Actor: "operator"}, invite))
	id, err := s.InvitationID(t.Context(), invite.Digest)
	requireOK(t, err)
	if id != invite.Public.ID {
		t.Fatal("digest selected another invitation")
	}
	for _, digest := range []string{"", "secret", strings.Repeat("c", 64)} {
		_, err := s.InvitationID(t.Context(), digest)
		requireCode(t, err, publicerr.Forbidden)
	}
	_, err = s.ReserveInvitation(t.Context(), stateport.InvitationReservation{ID: id, Digest: input.Digest, Token: input.Reservation, Until: s.options.Clock().Add(time.Minute)})
	requireOK(t, err)
	requireOK(t, s.Enroll(t.Context(), input))
	_, err = s.InvitationID(t.Context(), invite.Digest)
	requireCode(t, err, publicerr.Forbidden)
}
