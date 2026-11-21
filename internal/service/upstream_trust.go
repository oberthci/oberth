package service

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func (s *Service) persistUpstream(ctx context.Context, who actor.Actor, q apicontract.UpstreamAddArgs, prepared PreparedUpstream) error {
	if q.Repin {
		detail, err := json.Marshal(map[string][]string{"old": prepared.Replaced, "new": prepared.Offered})
		if err != nil {
			return err
		}
		if err = s.c.People.Admin.RecordAdministration(ctx, s.peopleAction(who, "upstream.changed", q.Reason, ""), model.AuditAction{Actor: who.Identity, Action: "upstream.repin", ResourceType: "upstream", ResourceID: q.Name, Details: string(detail)}); err != nil {
			return err
		}
	}
	if prepared.Persist != nil {
		return prepared.Persist(ctx)
	}
	return nil
}
