package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// stateport.InvitationReservation is an authenticated, expiring enrollment slot before CSR work.
// Token is caller-generated and never included in public audit details.

func (s *Store) ReserveInvitation(ctx context.Context, input stateport.InvitationReservation) (model.Invitation, error) {
	var invitation model.Invitation
	if !validDigest(input.Digest) || input.Token == "" || !input.Until.After(s.options.Clock()) {
		return invitation, refuse(invalid, nil)
	}
	err := s.observe(ctx, func(tx *sql.Tx) error {
		var body, token, until string
		err := tx.QueryRowContext(ctx, `SELECT value,reservation,reserved_until FROM invitations
   WHERE id=? AND digest=? AND consumed=0`, input.ID, input.Digest).Scan(&body, &token, &until)
		if err != nil {
			return refuse(forbidden, err)
		}
		if err := json.Unmarshal([]byte(body), &invitation); err != nil {
			return refuse(corrupt, err)
		}
		deadline, err := s.reservationAvailable(invitation, input, token, until)
		if err != nil {
			return err
		}
		if token == input.Token && deadline.Equal(input.Until) {
			return unchanged{}
		}
		_, err = tx.ExecContext(ctx, `UPDATE invitations SET reservation=?,reserved_until=? WHERE id=?`, input.Token,
			input.Until.UTC().Format(timestampFormat), input.ID)
		if err != nil {
			return databaseError(err)
		}
		return s.appendAudit(ctx, tx, mutation{invitation.Identity, "invitation.reserve", "invitation", input.ID, nil})
	}, input.Events...)
	return invitation, err
}

func (s *Store) reservationAvailable(invitation model.Invitation, input stateport.InvitationReservation, token, until string) (time.Time, error) {
	if !s.options.Clock().Before(invitation.ExpiresAt) || input.Until.After(invitation.ExpiresAt) {
		return time.Time{}, refuse(invitationExpired, nil)
	}
	deadline, _ := time.Parse(timestampFormat, until)
	if token != "" && token != input.Token && deadline.After(s.options.Clock()) {
		return deadline, refuse(invitationHeld, nil)
	}
	return deadline, nil
}

// InvitationID resolves a private handoff digest before the audited reservation.
// No public invitation data or signing work is returned before that reservation.
func (s *Store) InvitationID(ctx context.Context, digest string) (string, error) {
	if !validDigest(digest) {
		return "", refuse(forbidden, nil)
	}
	var id string
	err := s.readers.QueryRowContext(ctx, `SELECT id FROM invitations WHERE digest=? AND consumed=0`, digest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", refuse(forbidden, err)
	}
	return id, databaseError(err)
}
