package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// BindPublication fences an initially unknown predecessor exactly once.
func (s *Store) BindPublication(ctx context.Context, a stateport.Action, id, previous string) (model.Publication, error) {
	var p model.Publication
	err := s.change(ctx, mutation{a.Actor, "publication.bind", "publication", id, map[string]string{"previous_sha": previous, "reason": a.Reason}}, func(tx *sql.Tx) error {
		var err error
		p, err = readPublication(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Status != model.PublicationPending {
			return refuse(publicationSettled, nil)
		}
		if p.PreviousSHA != nil {
			if *p.PreviousSHA == previous {
				return unchanged{}
			}
			return refuse(conflict, nil)
		}
		if strings.HasPrefix(p.Ref, "refs/tags/") && previous != "" {
			return refuse(conflict, nil)
		}
		p.PreviousSHA = new(previous)
		p.UpdatedAt = s.options.Clock().UTC()
		return savePublication(ctx, tx, p)
	}, a.Events...)
	return p, err
}
func (s *Store) releaseSettlement(ctx context.Context, tx *sql.Tx, p *model.Publication, q stateport.PublicationResult) error {
	if q.Release == nil {
		return nil
	}
	v := *q.Release
	if !q.Delivered || v.PublicationID != p.ID || v.RunID != p.RunID || v.Tag != p.Ref || !strings.HasPrefix(p.Ref, "refs/tags/") {
		return refuse(invalid, nil)
	}
	r, err := loadRun(ctx, tx, p.RunID)
	if err != nil {
		return err
	}
	if !validReleaseRun(r, v.CommitSHA) || p.CommitSHA == nil || v.CommitSHA != *p.CommitSHA {
		return refuse(forbidden, nil)
	}
	p.Release = &v
	return nil
}

func validReleaseRun(r stateport.Run, sha string) bool {
	return r.Release && !r.Origin.HasAssistance() && r.Status == "passed" && r.CommitSHA != nil && sha == *r.CommitSHA
}
