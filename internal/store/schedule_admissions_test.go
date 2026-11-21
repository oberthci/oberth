package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func scheduleStore(t *testing.T) (*Store, admissioncontract.PreparedView, int64) {
	t.Helper()
	s := testStore(t)
	_, input := accepted(t, s, "seed")
	v := input.Prepared.View()
	v.RunID = ""
	v.Trigger = "schedule"
	v.ScheduleEntry = "nightly"
	v.SourceBindings = nil
	repo, err := s.Repository(t.Context(), v.Repository, nil)
	requireOK(t, err)
	return s, v, repo.ID
}

func TestScheduleRetentionRefusesNonScheduleAdmissions(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*admissioncontract.PreparedView){
		"run":     func(v *admissioncontract.PreparedView) { v.RunID = "run" },
		"trigger": func(v *admissioncontract.PreparedView) { v.Trigger = "branch" },
		"entry":   func(v *admissioncontract.PreparedView) { v.ScheduleEntry = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s, v, _ := scheduleStore(t)
			change(&v)
			requireCode(t, s.RetainSchedule(t.Context(), sealView(t, v)), publicerr.InvalidInput)
		})
	}
	s, _, _ := scheduleStore(t)
	requireCode(t, s.RetainSchedule(t.Context(), nil), publicerr.InvalidInput)
	requireCode(t, s.RetainSchedule(t.Context(), &admissioncontract.PreparedAdmission{}), publicerr.InvalidInput)
}

// A source-consuming schedule is retained with its sealed source bindings,
// which its children load and keep.
func TestScheduleRetentionKeepsSourceBindings(t *testing.T) {
	t.Parallel()
	s, v, id := scheduleStore(t)
	v.SourceBindings = []sourcecontract.Binding{{Path: "main"}}
	p := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), p))
	got, err := s.ScheduleAdmission(t.Context(), id, v.ScheduleEntry, p.View().Digest)
	requireOK(t, err)
	if len(got.SourceBindings) != 1 || got.SourceBindings[0].Path != "main" {
		t.Fatal("schedule admission lost its source bindings", got.SourceBindings)
	}
}

func TestScheduleRetentionKeepsCurrentAndPrevious(t *testing.T) {
	t.Parallel()
	s, v, id := scheduleStore(t)
	var admissions []*admissioncontract.PreparedAdmission
	var previous string
	for _, c := range []string{"a", "b", "c"} {
		v.CommitSHA = strings.Repeat(c, 40)
		admissions = append(admissions, sealView(t, v))
		requireOK(t, s.RetainSchedule(t.Context(), admissions[len(admissions)-1]))
		requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{admissions[len(admissions)-1].View().Digest, previous}))
		previous = admissions[len(admissions)-1].View().Digest
	}
	for range 3 {
		requireOK(t, s.RetainSchedule(t.Context(), admissions[2]))
		requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{admissions[2].View().Digest, admissions[1].View().Digest}))
	}
	_, err := s.ScheduleAdmission(t.Context(), id, "nightly", admissions[0].View().Digest)
	requireCode(t, err, publicerr.Forbidden)
	if !errors.As(err, new(stateport.ScheduleAdmissionExpiredError)) {
		t.Fatal("expiry refusal lost its kind", err)
	}
	for _, p := range admissions[1:] {
		got, err := s.ScheduleAdmission(t.Context(), id, "nightly", p.View().Digest)
		requireOK(t, err)
		if got.Digest != p.View().Digest {
			t.Fatal("retention changed admission")
		}
	}
	var count int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM schedule_admissions WHERE repo_id=? AND entry=?`, id, "nightly").Scan(&count))
	if count != 2 {
		t.Fatal("retention is unbounded", count)
	}
	// Reverting to the previous admission makes it current without losing its predecessor.
	requireOK(t, s.RetainSchedule(t.Context(), admissions[1]))
	requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{admissions[1].View().Digest, admissions[2].View().Digest}))
	v.CommitSHA = strings.Repeat("d", 40)
	next := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), next))
	requireOK(t, s.PublishSchedule(t.Context(), id, v.ScheduleEntry, []string{next.View().Digest, admissions[1].View().Digest}))
	_, err = s.ScheduleAdmission(t.Context(), id, "nightly", admissions[2].View().Digest)
	requireCode(t, err, publicerr.Forbidden)
	_, err = s.ScheduleAdmission(t.Context(), id, "nightly", admissions[1].View().Digest)
	requireOK(t, err)
}

func TestScheduleRetentionReadRefusesUnknownAndCorruptRows(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"json", "digest", "repository", "entry"} {
		t.Run(shape, func(t *testing.T) {
			s, v, id := scheduleStore(t)
			_, err := s.ScheduleAdmission(t.Context(), id, v.ScheduleEntry, strings.Repeat("f", 64))
			requireCode(t, err, publicerr.NotFound)
			p := sealView(t, v)
			requireOK(t, s.RetainSchedule(t.Context(), p))
			v = p.View()
			body := encode(v)
			switch shape {
			case "json":
				body = "{"
			case "digest":
				v.Digest = "wrong"
				body = encode(v)
			case "repository":
				v.Repository = "origin/team/other"
				body = encode(v)
			case "entry":
				v.ScheduleEntry = "other"
				body = encode(v)
			}
			_, err = s.writer.ExecContext(t.Context(), `UPDATE schedule_admissions SET value=? WHERE digest=?`, body, p.View().Digest)
			requireOK(t, err)
			_, err = s.ScheduleAdmission(t.Context(), id, "nightly", p.View().Digest)
			requireCode(t, err, publicerr.SchemaRefused)
		})
	}
}

func TestScheduleRetentionRemovalIsScopedAndPrunesFailedApply(t *testing.T) {
	t.Parallel()
	s, v, id := scheduleStore(t)
	first := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), first))
	v.ScheduleEntry = "other"
	other := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), other))
	repo, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/other")
	requireOK(t, err)
	v.Repository = repo.Canonical
	v.ScheduleEntry = "nightly"
	foreign := sealView(t, v)
	requireOK(t, s.RetainSchedule(t.Context(), foreign))
	requireOK(t, s.ForgetSchedule(t.Context(), id, "nightly"))
	_, err = s.ScheduleAdmission(t.Context(), id, "nightly", first.View().Digest)
	requireCode(t, err, publicerr.NotFound)
	_, err = s.ScheduleAdmission(t.Context(), id, "other", other.View().Digest)
	requireOK(t, err)
	_, err = s.ScheduleAdmission(t.Context(), repo.ID, "nightly", foreign.View().Digest)
	requireOK(t, err)
	requireOK(t, s.PruneSchedules(t.Context(), id, []string{"other"}))
	_, err = s.ScheduleAdmission(t.Context(), id, "other", other.View().Digest)
	requireOK(t, err)
	requireOK(t, s.PruneSchedules(t.Context(), id, nil))
	_, err = s.ScheduleAdmission(t.Context(), id, "other", other.View().Digest)
	requireCode(t, err, publicerr.NotFound)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.RetainSchedule(ctx, other); err == nil {
		t.Fatal("cancelled write committed")
	}
}
