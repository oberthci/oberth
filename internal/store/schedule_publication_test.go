package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSchedulePublicationRequiresRetainedEntry(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"repository", "entry", "digest", "empty"} {
		t.Run(shape, func(t *testing.T) {
			s, v, id := scheduleStore(t)
			p := sealView(t, v)
			requireOK(t, s.RetainSchedule(t.Context(), p))
			entry, digest := v.ScheduleEntry, p.View().Digest
			switch shape {
			case "repository":
				id++
			case "entry":
				entry = "other"
			case "digest":
				digest = strings.Repeat("f", 64)
			case "empty":
				digest = ""
			}
			code := publicerr.NotFound
			if shape == "empty" {
				code = publicerr.InvalidInput
			}
			requireCode(t, s.PublishSchedule(t.Context(), id, entry, []string{digest, digest}), code)
			var count int
			requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM schedule_admissions`).Scan(&count))
			if count != 1 {
				t.Fatal("refused publication pruned snapshots", count)
			}
		})
	}
}

func TestRemoveUnfiredRepositoryCascadesScheduleAdmissions(t *testing.T) {
	t.Parallel()
	s, v, _ := scheduleStore(t)
	repo, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/unused")
	requireOK(t, err)
	v.Repository, v.RunID, v.Trigger, v.ScheduleEntry, v.SourceBindings = repo.Canonical, "", "schedule", "nightly", nil
	p := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), p))
	requireOK(t, s.PublishSchedule(t.Context(), repo.ID, v.ScheduleEntry, []string{p.View().Digest, ""}))
	requireOK(t, s.RemoveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID))
	var count int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM schedule_admissions WHERE repo_id=?`, repo.ID).Scan(&count))
	if count != 0 {
		t.Fatal("removed repository retained snapshots", count)
	}
}

func TestFailedRepositoryRemovalKeepsScheduleAdmissions(t *testing.T) {
	t.Parallel()
	s, v, id := scheduleStore(t)
	p := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), p))
	requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{p.View().Digest, ""}))
	if err := s.RemoveRepository(t.Context(), stateport.Action{Actor: "operator"}, id); err == nil {
		t.Fatal("repository history was removed")
	}
	_, err := s.ScheduleAdmission(t.Context(), id, v.ScheduleEntry, p.View().Digest)
	requireOK(t, err)
}

func TestSchedulePublicationRefusesInvalidKeepSet(t *testing.T) {
	t.Parallel()
	for _, keep := range [][]string{nil, {""}, {"", "previous"}, {"current", "previous", "older"}} {
		s, _, id := scheduleStore(t)
		requireCode(t, s.PublishSchedule(t.Context(), id, "nightly", keep), publicerr.InvalidInput)
	}
}

func TestUnchangedSchedulePublicationExcludesUnpublishedRows(t *testing.T) {
	t.Parallel()
	s, v, id := scheduleStore(t)
	published := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), published))
	requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{published.View().Digest, ""}))
	var pending []string
	for _, c := range []string{"b", "c"} {
		v.CommitSHA = strings.Repeat(c, 40)
		p := sealView(t, v)
		requireOK(t, s.RetainSchedule(t.Context(), p))
		pending = append(pending, p.View().Digest)
	}
	requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{published.View().Digest, ""}))
	for _, digest := range pending {
		_, err := s.ScheduleAdmission(t.Context(), id, v.ScheduleEntry, digest)
		requireCode(t, err, publicerr.Forbidden)
	}
}
