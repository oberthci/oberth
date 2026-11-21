package store

import (
	"github.com/oberthci/oberth/internal/stateport"
)

func administrativeQuery(scope []string, f stateport.AdministrativeFilter) (string, []any, error) {
	since, until, err := f.Bounds()
	if err != nil {
		return "", nil, err
	}
	where, args := administrativeScope(scope)
	if f.Before > 0 {
		where += ` AND a.id<?`
		args = append(args, f.Before)
	}
	for _, filter := range []struct{ column, value string }{{"action", f.Action}, {"actor", f.Actor}} {
		if filter.value != "" {
			where += ` AND a.` + filter.column + `=?`
			args = append(args, filter.value)
		}
	}
	if !since.IsZero() {
		where += ` AND a.created_at>=?`
		args = append(args, since.UTC().Format(timestampFormat))
	}
	if !until.IsZero() {
		where += ` AND a.created_at<=?`
		args = append(args, until.UTC().Format(timestampFormat))
	}
	query := `SELECT a.id,a.actor,a.action,a.resource_type,a.resource_id,a.details,a.created_at,a.previous,a.hash FROM audit a WHERE ` + where + ` ORDER BY a.id DESC LIMIT ?`
	return query, append(args, f.Limit), nil
}

func administrativeScope(scope []string) (string, []any) {
	if scope == nil {
		return "1=1", nil
	}
	direct, args := scopePredicate("a.resource_id", scope)
	runs, runArgs := scopePredicate("r.canonical", scope)
	return `((a.resource_type IN ('repository','authority','capability','schedule') AND ` + direct + `) OR
 (a.resource_type='run' AND EXISTS(SELECT 1 FROM runs x JOIN repositories r ON r.id=x.repo_id WHERE x.id=a.resource_id AND ` + runs + `)))`, append(args, runArgs...)
}
