package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestScheduleDelegatesAuthorizedFacts(t *testing.T) {
	t.Parallel()
	p := people(t)
	recorder := &scheduleRecorder{t: t}
	p.f.c.People.Schedules = &testSchedules{Act: recorder.Act, Read: recorder.Read}
	p.f.restart(t)
	pause, e := p.f.s.PrepareSchedulePause(t.Context(), adminCaller(), apicontract.SchedulePauseArgs{Repo: "widget", Entry: "nightly", Reason: "operator request"})
	if _, e = execute(t.Context(), pause, e); e != nil {
		t.Fatal(e)
	}
	resume, e := p.f.s.PrepareScheduleResume(t.Context(), adminCaller(), apicontract.ScheduleResumeArgs{Repo: repository, Entry: "nightly", Reason: "operator request"})
	if _, e = execute(t.Context(), resume, e); e != nil {
		t.Fatal(e)
	}
	trigger, e := p.f.s.PrepareScheduleTrigger(t.Context(), adminCaller(), apicontract.ScheduleTriggerArgs{Repo: repository, Entry: "nightly", Reason: "operator request", Force: true})
	if _, e = execute(t.Context(), trigger, e); e != nil {
		t.Fatal(e)
	}
	list, e := p.f.s.PrepareScheduleList(t.Context(), workerCaller(), apicontract.ScheduleListArgs{Repo: repository})
	page, e := execute(t.Context(), list, e)
	if e != nil || page.Schedules == nil || recorder.Calls != 3 {
		t.Fatal(page, e, recorder.Calls)
	}
}
func TestReceiveRepairDelegatesScopedAttribution(t *testing.T) {
	t.Parallel()
	p := people(t)
	calls := 0
	p.f.c.People.Administration = &testAdministration{Outbox: func(a stateport.Action, scope []string, q apicontract.ReceiveOutboxArgs) (apicontract.OutboxResult, error) {
		calls++
		if a.Actor != admin().Identity || a.Reason != "recovery" || len(scope) != 1 || scope[0] != repository || q.Repo != repository || q.ID != "receive-1" || q.Action != "retry" {
			t.Fatal("unscoped or unattributed repair", a, scope, q)
		}
		return apicontract.OutboxResult{}, nil
	}}
	p.f.restart(t)
	call, e := p.f.s.PrepareReceiveOutbox(t.Context(), adminCaller(), apicontract.ReceiveOutboxArgs{Action: "retry", Repo: "widget", ID: "receive-1", Reason: "recovery"})
	v, e := execute(t.Context(), call, e)
	if e != nil || v.Records == nil || calls != 1 {
		t.Fatal(v, e, calls)
	}
}

type scheduleRecorder struct {
	t     *testing.T
	Calls int
}

func (p *scheduleRecorder) Act(a stateport.Action, c service.ScheduleControl, paused bool) (apicontract.Receipt, error) {

	p.Calls++
	if a.Actor != admin().Identity || a.Reason != "operator request" || c.Repository.Canonical != repository || c.Entry != "nightly" {
		p.t.Fatal("lost attribution or identity", a, c)
	}
	if p.Calls == 1 && !paused {
		p.t.Fatal("pause inverted")
	}
	if p.Calls == 3 && !c.Force {
		p.t.Fatal("manual pause override lost")
	}
	return apicontract.Receipt{ID: c.Entry}, nil

}
func (p *scheduleRecorder) Read(scope []string) (apicontract.SchedulePage, error) {
	if len(scope) != 1 || scope[0] != repository {
		p.t.Fatal("scope not forwarded", scope)
	}
	return apicontract.SchedulePage{}, nil
}
