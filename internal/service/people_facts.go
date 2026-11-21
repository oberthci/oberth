package service

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
)

func publicIssueError(err error) *publicerr.Error { return publicerr.Classify(err) }
func (s *Service) peopleAction(a actor.Actor, kind, reason, repo string) stateport.Action {
	action := stateport.Action{Actor: a.Identity, Caller: a.Clone(), Reason: reason}
	if strings.HasPrefix(kind, "issue.") {
		kind = string(model.EventIssueUpdated)
		action.Events = []stateport.EventWrite{{Event: model.AgentEvent{Kind: kind, At: s.c.Now(), Repo: repo, Summary: reasonIssueUpdated}, Dedupe: fmt.Sprintf("%s:%s", kind, s.c.NewID())}}
	}
	return action
}
func projectCI(r stateport.Run, o FinishedRun) bool {
	if r.Origin.HasAssistance() || r.Origin.Trigger != model.TriggerBranch && r.Origin.Trigger != model.TriggerSchedule {
		return false
	}
	if o.Status != stepmodel.RunPassed && o.Status != stepmodel.RunFailed {
		return false
	}
	if o.Reason == stateport.ReasonRestart {
		return false
	}
	return o.Failure == nil || o.Failure.Class == stepmodel.FailureAuthor
}

func (p People) validate() error {
	ports := []any{p.Issues, p.Enrollment, p.Grants, p.Publication, p.Cluster, p.Administration, p.Admin, p.Issuer, p.Backup, p.Random, p.Schedules}
	present := 0
	for _, port := range ports {
		if port != nil {
			present++
		}
	}
	if present == 0 {
		return nil
	}
	if present != len(ports) || p.CAPin == "" || p.PublicURL == "" {
		return refuse(unavailable)
	}
	return p.BackupOptions.Validate()
}
func (s *Service) peopleAllow(ctx context.Context, a actor.Actor, action string) (actor.Actor, error) {
	current, err := s.allow(ctx, a, action)
	if err != nil {
		return current, err
	}
	if s.c.People.Issuer == nil {
		return actor.Actor{}, refuse(unavailable)
	}
	return current, nil
}

func publicScope(scope []string) []string { return append([]string{}, scope...) }
