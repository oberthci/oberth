package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/stateport"
)

// PreparePublication persists the immutable exact push intent after testing.
func (s *Store) PreparePublication(ctx context.Context, action stateport.Action, input stateport.PublicationIntent) error {
	return s.observe(ctx, func(tx *sql.Tx) error { return s.preparePublication(ctx, tx, input, action) }, action.Events...)
}

func (s *Store) preparePublication(ctx context.Context, tx *sql.Tx, input stateport.PublicationIntent, action stateport.Action) error {
	actor := action.Actor
	p := input.Publication
	if err := validatePublication(p, actor); err != nil {
		return err
	}
	if p.RunID == "" {
		return s.writePublication(ctx, tx, p, action.Reason)
	}
	r, err := loadRun(ctx, tx, p.RunID)
	if err != nil {
		return err
	}
	if r.Origin.HasAssistance() {
		return refuse(assistedEvidence, nil)
	}
	if r.Repo != p.Repo || r.ObjectSHA != p.ObjectSHA || r.Status != "passed" {
		return refuse(forbidden, nil)
	}
	if p.CommitSHA == nil {
		p.CommitSHA = r.CommitSHA
	}
	if r.CommitSHA == nil || *p.CommitSHA != *r.CommitSHA {
		return refuse(forbidden, nil)
	}
	return s.writePublication(ctx, tx, p, action.Reason)
}

func (s *Store) writePublication(ctx context.Context, tx *sql.Tx, p model.Publication, reason string) error {
	repoID, err := repositoryNumber(ctx, tx, p.Repo)
	if err != nil {
		return err
	}
	p.Status = model.PublicationPending
	p.Attempts = 0
	p.RetryAt = nil
	p.Reason = ""
	p.CreatedAt = s.options.Clock().UTC()
	p.UpdatedAt = p.CreatedAt
	var runID any
	if p.RunID != "" {
		runID = p.RunID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO publications VALUES(?,?,?,?,?)`, p.ID, repoID, runID, p.Status, encode(storedPublication{Publication: p, Attempted: new(false), ReceivedHeads: p.ReceivedHeads})); err != nil {
		return refuse(conflict, err)
	}
	return s.appendAudit(ctx, tx, mutation{p.Actor, "publication.prepare", "publication", p.ID, map[string]any{"publication": p, "reason": reason}})
}

// StartPublicationAttempt commits its CAS before a push.
func (s *Store) StartPublicationAttempt(ctx context.Context, action stateport.Action, attempt stateport.PublicationAttempt) (model.Publication, error) {
	var p model.Publication
	err := s.change(ctx, mutation{action.Actor, "publication.attempt", "publication", attempt.ID, map[string]any{"attempt": attempt.ExpectedAttempts + 1, "reason": action.Reason}}, func(tx *sql.Tx) error {
		var err error
		p, err = readPublication(ctx, tx, attempt.ID)
		if err != nil {
			return err
		}
		if err := canStartPublication(p, attempt, s.options.Clock()); err != nil {
			return err
		}
		if !p.Attempted {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM runs`).Scan(&p.CIAfter); err != nil {
				return databaseError(err)
			}
			p.Attempted = true
		}
		p.Attempts++
		p.RetryAt = nil
		p.UpdatedAt = s.options.Clock().UTC()
		return savePublication(ctx, tx, p)
	}, action.Events...)
	return p, err
}

// SettlePublication records external delivery and the durable retry outcome.
func (s *Store) SettlePublication(ctx context.Context, action stateport.Action, result stateport.PublicationResult) error {
	return s.change(ctx, mutation{action.Actor, "publication.settle", "publication", result.ID, map[string]any{"result": result, "reason": action.Reason}}, func(tx *sql.Tx) error {
		p, err := readPublication(ctx, tx, result.ID)
		if err != nil {
			return err
		}
		if p.Status != model.PublicationPending || p.Attempts != result.Attempt || result.Attempt < 1 {
			return refuse(attemptChanged, nil)
		}
		if err := s.releaseSettlement(ctx, tx, &p, result); err != nil {
			return err
		}
		p.Reason = result.Reason
		p.Detail = result.Detail
		p.RetryAt = nil
		p.UpdatedAt = s.options.Clock().UTC()
		if result.Delivered {
			p.Status = model.PublicationDelivered
			if result.ProjectCI {
				if err := s.projectPublished(ctx, tx, p); err != nil {
					return err
				}
			}
		} else if result.Retryable && p.Attempts < pushretry.MaxAttempts {
			p.RetryAt = new(p.UpdatedAt.Add(pushretry.Delay(p.Attempts)))
		} else {
			p.Status = model.PublicationFailed
		}
		return savePublication(ctx, tx, p)
	}, action.Events...)
}

func (s *Store) AbandonPublication(ctx context.Context, action stateport.Action, id string) error {
	return s.change(ctx, mutation{action.Actor, "publication.abandon", "publication", id, map[string]string{"reason": action.Reason}}, func(tx *sql.Tx) error {
		p, err := readPublication(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Status != model.PublicationPending && p.Status != model.PublicationFailed {
			return refuse(publicationSettled, nil)
		}
		p.Reason = action.Reason
		p.Status = model.PublicationAbandoned
		p.RetryAt = nil
		p.UpdatedAt = s.options.Clock().UTC()
		return savePublication(ctx, tx, p)
	}, action.Events...)
}

// storedPublication keeps private attempt and receive evidence in the existing
// row JSON, outside the record's public encoding.
type storedPublication struct {
	model.Publication
	CIAfter       int64    `json:"ci_after,omitempty"`
	Attempted     *bool    `json:"attempted,omitempty"`
	ReceivedHeads []string `json:"received_heads,omitempty"`
}

func readPublication(ctx context.Context, tx *sql.Tx, id string) (model.Publication, error) {
	var stored storedPublication
	var body string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM publications WHERE id=?`, id).Scan(&body); err != nil {
		return stored.Publication, databaseError(err)
	}
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		return stored.Publication, refuse(corrupt, err)
	}
	return stored.publication(), nil
}

func (stored storedPublication) publication() model.Publication {
	// Existing schema 6 rows used ci_after as both ordering and attempt evidence.
	// Decode that private representation once; every new write separates them.
	attempted := stored.Attempts > 0 || stored.CIAfter != 0
	if stored.Attempted != nil {
		attempted = *stored.Attempted
	}
	stored.Publication.CIAfter = max(0, stored.CIAfter)
	stored.Publication.Attempted = attempted
	stored.Publication.ReceivedHeads = stored.ReceivedHeads
	return stored.Publication
}

func savePublication(ctx context.Context, tx *sql.Tx, p model.Publication) error {
	value := encode(storedPublication{Publication: p, CIAfter: p.CIAfter, Attempted: new(p.Attempted), ReceivedHeads: p.ReceivedHeads})
	_, err := tx.ExecContext(ctx, `UPDATE publications SET status=?,value=? WHERE id=?`, p.Status, value, p.ID)
	return databaseError(err)
}

func (s *Store) Publication(ctx context.Context, id string) (model.Publication, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Publication{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readPublication(ctx, tx, id)
	return p, err
}

func validatePublication(p model.Publication, actor string) error {
	if actor == "" || p.ID == "" || p.Actor != actor || !validRef(p.Ref) || p.ObjectSHA == "" {
		return refuse(invalid, nil)
	}
	if strings.HasPrefix(p.Ref, "refs/tags/") && p.PreviousSHA != nil && *p.PreviousSHA != "" {
		return refuse(invalid, nil)
	}
	return nil
}

func canStartPublication(p model.Publication, attempt stateport.PublicationAttempt, now time.Time) error {
	if p.Status != model.PublicationPending {
		return refuse(publicationSettled, nil)
	}
	if p.Attempts >= pushretry.MaxAttempts {
		return refuse(attemptChanged, nil)
	}
	if p.Attempts != attempt.ExpectedAttempts {
		return refuse(attemptChanged, nil)
	}
	if p.Attempts > 0 && p.RetryAt == nil {
		return refuse(attemptInFlight, nil)
	}
	if p.RetryAt != nil && now.Before(*p.RetryAt) {
		return refuse(retryDeferred, nil)
	}
	return nil
}
