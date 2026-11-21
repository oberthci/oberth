package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestAuditMissingTailAndHeadMismatchRefuse(t *testing.T) {
	t.Parallel()
	for _, damage := range []string{"tail", "head", "predecessor", "gap"} {
		t.Run(damage, func(t *testing.T) {
			s := testStore(t)
			requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
			requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "second", Org: "other", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
			var update, remove string
			requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='audit_immutable'`).Scan(&update))
			requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE name='audit_retained'`).Scan(&remove))
			execute(t, s.writer, `DROP TRIGGER audit_immutable`)
			execute(t, s.writer, `DROP TRIGGER audit_retained`)
			switch damage {
			case "tail":
				execute(t, s.writer, `DELETE FROM audit WHERE id=2`)
			case "head":
				execute(t, s.writer, `UPDATE audit_head SET hash='`+strings.Repeat("a", 64)+`'`)
			case "predecessor":
				execute(t, s.writer, `UPDATE audit SET previous='`+strings.Repeat("b", 64)+`' WHERE id=2`)
				var action model.AuditAction
				var timestamp string
				requireOK(t, s.writer.QueryRowContext(t.Context(), `SELECT id,actor,action,resource_type,resource_id,details,created_at FROM audit WHERE id=2`).Scan(&action.ID, &action.Actor, &action.Action, &action.ResourceType, &action.ResourceID, &action.Details, &timestamp))
				var err error
				action.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
				requireOK(t, err)
				hash := stateport.AuditDigest(s.options.Protocol.AuditDomain, strings.Repeat("b", 64), action)
				_, err = s.writer.ExecContext(t.Context(), `UPDATE audit SET hash=? WHERE id=2`, hash)
				requireOK(t, err)
				_, err = s.writer.ExecContext(t.Context(), `UPDATE audit_head SET hash=?`, hash)
				requireOK(t, err)

			case "gap":
				execute(t, s.writer, `DELETE FROM audit WHERE id=1`)
			}
			execute(t, s.writer, update)
			execute(t, s.writer, remove)
			_, _, err := s.Audit(t.Context(), 0, 200)
			requireCode(t, err, publicerr.SchemaRefused)
			_, err = s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
			requireCode(t, err, publicerr.SchemaRefused)
		})
	}
}
