package main

import (
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func (o *serverOwners) scheduleConfig() {
	o.c.scheduleConfig = cronsync.Config{InstanceID: o.c.config.Namespace, Client: o.cluster.Argo.ArgoprojV1alpha1().CronWorkflows(o.c.config.PipelineNamespace), Git: scheduleSource{cacheSource{o}}, PauseIntent: o.c.store.SchedulePaused, Repositories: o.scheduleRepositories, Repository: o.scheduleRepository, RepositoryAttempt: repositoryAttempt, Admit: func(ctx context.Context, p cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		return o.wiring.PrepareSchedule(ctx, app.Request{RepositoryID: p.Repository.ID, Repository: p.Repository.Qualified, Ref: p.Revision, Trigger: model.TriggerSchedule, ScheduleEntry: p.Entry.Name, Actor: actor.Actor{Identity: "schedule", AuthMethod: "system"}})
	}}
}
func (o *serverOwners) scheduleRepositories(ctx context.Context) ([]cronsync.Repository, error) {
	rows, err := o.repositories(ctx, nil)
	out := make([]cronsync.Repository, 0, len(rows))
	for _, r := range rows {
		out = append(out, o.scheduleRepo(r))
	}
	return out, err
}
func (o *serverOwners) repositories(ctx context.Context, scope []string) ([]stateport.Repository, error) {
	result := []stateport.Repository{}
	before := int64(0)
	for {
		rows, err := o.c.store.Repositories(ctx, stateport.CatalogFilter{Repositories: scope, Before: before, Limit: 200, IncludeArchived: true})
		if err != nil {
			return nil, err
		}
		result = append(result, rows...)
		if len(rows) < 200 {
			return result, nil
		}
		before = rows[len(rows)-1].ID
	}
}
func (o *serverOwners) scheduleRepo(r stateport.Repository) cronsync.Repository {
	return cronsync.Repository{ID: r.ID, Qualified: r.Canonical, DefaultBranch: strings.TrimPrefix(o.settings.DefaultBranch, "refs/heads/"), Archived: r.Archived}
}
func (o *serverOwners) scheduleRepository(ctx context.Context, key string) (cronsync.Repository, bool, error) {
	r, err := o.c.store.Repository(ctx, key, nil)
	if missingStore(err) {
		return cronsync.Repository{}, false, nil
	}
	return o.scheduleRepo(r), err == nil, err
}

type serverSchedules struct{ owner *serverOwners }

// List prints each repository's schedules as they are. For a repository with
// an entry waiting for its branch it first looks at the forge (lookAgain) and
// hands what moved to the ref-notice queue, so that reconciliation runs after
// the list returns: a list right after a branch reached the forge may still
// show the entry waiting, and a later list shows it admitted. Nothing extra is
// printed for an entry still waiting.
func (s serverSchedules) List(ctx context.Context, scope []string) (apicontract.SchedulePage, error) {
	rows, err := s.owner.repositories(ctx, scope)
	if err != nil {
		return apicontract.SchedulePage{}, err
	}
	out := apicontract.SchedulePage{Schedules: []apicontract.Schedule{}}
	// One look deadline for the whole list: repositories not looked at in time
	// keep their entries as they are.
	look, cancel := context.WithTimeout(ctx, lookBound)
	defer cancel()
	for _, r := range rows {
		statuses, err := s.owner.c.schedules.Snapshot(ctx, s.owner.scheduleRepo(r))
		if err == nil && look.Err() == nil && slices.ContainsFunc(statuses, cronsync.BranchMissing) {
			s.lookAgain(look, r)
		}
		if err != nil {
			return out, err
		}
		for _, v := range statuses {
			out.Schedules = append(out.Schedules, apicontract.Schedule{Repo: r.Canonical, Entry: v.Entry, Paused: v.UserPaused, DisplayReason: v.Error, Detail: v.Detail})
		}
	}
	return out, nil
}

// lookBound keeps a look well inside a read's budget, so the read answers
// even when the forge does not.
const lookBound = 5 * time.Second

// lookAgain asks the forge, within the caller's look deadline and at most once
// per repository per freshness interval (the cache's look marker), for refs the
// cache lacks, and hands the refs that moved to the ref-notice queue: its
// reconciliation runs under the server's lifetime and its own bound, not in what
// is left of the look. It reports whether a ref moved. A forge that does not
// answer, or answers with nothing new, queues nothing and leaves every entry as
// it was; that outcome is the server's notice, not the caller's error.
func (s serverSchedules) lookAgain(ctx context.Context, r stateport.Repository) bool {
	if err := s.owner.cache.Init(ctx, r.Canonical); err != nil {
		s.owner.notice(err)
		return false
	}
	moved, err := s.owner.cache.LookForMissing(ctx, r.Canonical)
	s.owner.notice(err)
	if len(moved) == 0 {
		return false
	}
	s.owner.notices.changed(ctx, r.Canonical, moved)
	return true
}
func (s serverSchedules) SetPaused(ctx context.Context, a stateport.Action, c service.ScheduleControl, paused bool) (apicontract.Receipt, error) {
	err := s.owner.c.store.PauseSchedule(ctx, a, store.SchedulePause{Repository: c.Repository.Canonical, Entry: c.Entry, Paused: paused})
	if err == nil {
		err = s.owner.c.schedules.SetPause(ctx, s.owner.scheduleRepo(c.Repository), c.Entry)
	}
	state := "resumed"
	if paused {
		state = "paused"
	}
	return apicontract.Receipt{ID: c.Entry, State: state}, err
}
func (s serverSchedules) Trigger(ctx context.Context, a stateport.Action, c service.ScheduleControl) (apicontract.Receipt, error) {
	paused, err := s.owner.c.store.SchedulePaused(ctx, c.Repository.Canonical, c.Entry)
	if err != nil {
		return apicontract.Receipt{}, err
	}
	if paused && !c.Force {
		return apicontract.Receipt{}, publicerr.New(publicerr.Conflict, "The schedule is paused.", "Resume it or explicitly force one occurrence.", nil)
	}
	status, err := s.owner.c.schedules.Status(ctx, s.owner.scheduleRepo(c.Repository), c.Entry)
	if err == nil && cronsync.BranchMissing(status) {
		// A ref that moved is reconciled by the queue; wait for it, inside the
		// same look deadline, before deciding on the entry.
		look, cancel := context.WithTimeout(ctx, lookBound)
		if s.lookAgain(look, c.Repository) {
			s.owner.notice(s.owner.notices.settled(look, c.Repository.Canonical))
		}
		cancel()
		status, err = s.owner.c.schedules.Status(ctx, s.owner.scheduleRepo(c.Repository), c.Entry)
	}
	if err != nil {
		return apicontract.Receipt{}, err
	}
	if status.Error != "" || status.SyncSuspended {
		return apicontract.Receipt{}, cronsync.NotAdmitted(status)
	}
	ref, err := cacheSource(s).ResolveRef(ctx, c.Repository.Canonical, status.Branch)
	if err != nil {
		return apicontract.Receipt{}, err
	}
	if err = s.owner.c.store.RecordAdministration(ctx, a, model.AuditAction{Actor: a.Actor, Action: "schedule.trigger", ResourceType: "schedule", ResourceID: c.Entry}); err != nil {
		return apicontract.Receipt{}, err
	}
	return s.owner.wiring.StartRun(ctx, service.StartRequest{Request: model.CIRequest{ReceiveID: rand.Text(), Repo: c.Repository.Canonical, Ref: ref.Name, ObjectSHA: ref.ObjectSHA, CommitSHA: &ref.CommitSHA, Actor: actor.Actor{Identity: a.Actor, AuthMethod: "system"}, Trigger: model.TriggerSchedule, ScheduleEntry: c.Entry}, Reason: a.Reason})
}
