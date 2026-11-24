package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type countedScheduledReads struct {
	service.RunStore
	calls *int
}

func (r countedScheduledReads) LookupRun(ctx context.Context, scope []string, id string) (stateport.Run, error) {
	*r.calls++
	return r.RunStore.LookupRun(ctx, scope, id)
}

func (r countedScheduledReads) ResolveRun(ctx context.Context, scope []string, id string) (stateport.Run, error) {
	*r.calls++
	return r.RunStore.ResolveRun(ctx, scope, id)
}
func (r countedScheduledReads) RunByRef(ctx context.Context, scope []string, ref string) (stateport.Run, error) {
	*r.calls++
	return r.RunStore.RunByRef(ctx, scope, ref)
}

func finishScheduledOperation(t *testing.T, f *fixture, name, id string) {
	t.Helper()
	status := stepmodel.RunPassed
	if name == "rerun" || name == "purge" {
		status = stepmodel.RunFailed
	} else if !strings.HasPrefix(name, "wait") {
		return
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: id, Status: status}); err != nil {
		t.Fatal(err)
	}
}

func scheduledAgentRefusal(name string) publicerr.Code {
	switch name {
	case "cancel", "purge":
		return publicerr.Forbidden
	case "rerun":
		return publicerr.InvalidTransition
	default:
		return ""
	}
}

func scheduledRerun(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
	t.Helper()
	starts := 0
	f.starter.Start = func(_ context.Context, q service.StartRequest) (apicontract.Receipt, error) {
		starts++
		if q.RerunOf != id || q.Request.Trigger != model.TriggerSchedule || q.Request.ScheduleEntry != "daily" {
			t.Fatal("scheduled rerun lost its origin", q)
		}
		return apicontract.Receipt{ID: "new-run", State: "running"}, nil
	}
	call, err := f.s.PrepareRunRerun(t.Context(), c, apicontract.RunRerunArgs{ID: id})
	out, err := execute(t.Context(), call, err)
	if err == nil && (out.ID != "new-run" || starts != 1) {
		t.Fatal("scheduled rerun did not start", out, starts)
	}
	return id, err
}

func scheduledEvents(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
	t.Helper()
	call, err := f.s.PrepareEventsWait(t.Context(), c, apicontract.EventsWaitArgs{AfterID: new(int64), RunIDs: []string{id}})
	out, err := execute(t.Context(), call, err)
	if err != nil {
		return "", err
	}
	if len(out.Events) == 0 {
		t.Fatal("scheduled run filter returned no retained event", out)
	}
	for _, event := range out.Events {
		if event.RunID != id {
			t.Fatal("scheduled run filter leaked another run", event)
		}
	}
	return id, nil
}
