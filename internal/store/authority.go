package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// SecretGrant changes one exact approved tuple; declarations stay in admission.
func (s *Store) SecretGrant(ctx context.Context, action stateport.Action, grant workflowpolicy.GrantKey, revoke bool) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	return s.change(ctx, mutation{action.Actor, "secret.grant", "secret", encode(grant), grant}, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, grant.Repository)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO secret_grants(repo_id,file,step,path,audience,revoked) VALUES(?,?,?,?,?,?)
   ON CONFLICT(repo_id,file,step,path,audience) DO UPDATE SET revoked=excluded.revoked`,
			id, grant.File, grant.Step, grant.Path, grant.Audience, revoke)
		return databaseError(err)
	}, action.Events...)
}

const maxCapabilityBytes = 16 << 10

// CapabilityGrant contains only bounded public configuration.
type CapabilityGrant struct {
	Repository string
	JSON       []byte
	Revoke     bool
}

func (s *Store) Capability(ctx context.Context, action stateport.Action, grant CapabilityGrant) error {
	if !validRepository(grant.Repository) || len(grant.JSON) > maxCapabilityBytes || !json.Valid(grant.JSON) {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{
		action.Actor,
		"capability.grant",
		"capability",
		grant.Repository,
		json.RawMessage(grant.JSON),
	}, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, grant.Repository)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO capabilities VALUES(?,?,?)
   ON CONFLICT(repo_id) DO UPDATE SET value=excluded.value,revoked=excluded.revoked`, id, string(grant.JSON), grant.Revoke)
		return databaseError(err)
	}, action.Events...)
}

// ClaimOwner is permanent repository/tier ownership, independent of access revocation.
type ClaimOwner struct{ Claim, Repository, Tier string }

func (s *Store) GrantClaim(ctx context.Context, action stateport.Action, owner ClaimOwner) error {
	if owner.Claim == "" || !validRepository(owner.Repository) || (owner.Tier != "ci" && owner.Tier != "release") {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "claim.grant", "claim", owner.Claim, owner}, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, owner.Repository)
		if err != nil {
			return err
		}
		var previous int64
		var tier string
		err = tx.QueryRowContext(ctx, `SELECT repo_id,tier FROM pvc_owners WHERE claim=?`, owner.Claim).Scan(&previous, &tier)
		if err == nil {
			if previous != id || tier != owner.Tier {
				return refuse(claimBound, nil)
			}
			return unchanged{}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return databaseError(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO pvc_owners VALUES(?,?,?)`, owner.Claim, id, owner.Tier)
		return databaseError(err)
	}, action.Events...)
}

type SchedulePause struct {
	Repository, Entry string
	Paused            bool
}

func (s *Store) PauseSchedule(ctx context.Context, action stateport.Action, input SchedulePause) error {
	if !validRepository(input.Repository) || input.Entry == "" {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "schedule.pause", "schedule", input.Repository, input}, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, input.Repository)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schedule_pauses VALUES(?,?,?)
   ON CONFLICT(repo_id,entry) DO UPDATE SET paused=excluded.paused`, id, input.Entry, input.Paused)
		return databaseError(err)
	}, action.Events...)
}
