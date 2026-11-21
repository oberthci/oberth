package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestDirectZeroTimeoutUsesTransportDefault(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"run_wait", "wait", "events_wait", "promote_status"} {
		t.Run(name, func(t *testing.T) { synctest.Test(t, func(t *testing.T) { checkZeroTimeout(t, name) }) })
	}
}
func checkZeroTimeout(t *testing.T, name string) {
	t.Helper()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	p := model.Promotion{ID: "pending", Repo: repository, Actor: caller().Identity, Caller: caller(), ResultSHA: head}
	if err := f.store.PreparePromotion(t.Context(), stateport.Action{Actor: p.Actor}, p); err != nil {
		t.Fatal(err)
	}
	field, _ := reflect.TypeFor[apicontract.RunWaitArgs]().FieldByName("TimeoutS")
	seconds, err := strconv.Atoi(field.Tag.Get("default"))
	if err != nil || seconds < 1 {
		t.Fatal("transport has no positive default", err)
	}
	done := make(chan error, 1)
	go func() { done <- callZeroTimeout(t, f, name) }()
	synctest.Wait()
	select {
	case err := <-done:
		t.Fatal("zero timeout completed immediately", err)
	default:
	}
	timer := time.NewTimer(time.Duration(seconds-1) * time.Second)
	<-timer.C
	synctest.Wait()
	select {
	case err := <-done:
		t.Fatal("timeout shorter than advertised", err)
	default:
	}
	timer.Reset(time.Second)
	<-timer.C
	synctest.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("timeout longer than advertised")
	}
}

func callZeroTimeout(t *testing.T, f *fixture, name string) error {
	t.Helper()
	switch name {
	case "run_wait":
		call, err := f.s.PrepareRunWait(t.Context(), workerCaller(), apicontract.RunWaitArgs{ID: runID})
		_, err = execute(t.Context(), call, err)
		return err
	case "wait":
		call, err := f.s.Service.PrepareWait(t.Context(), workerCaller(), apicontract.WaitArgs{ID: runID})
		_, err = execute(t.Context(), call, err)
		return err
	case "events_wait":
		call, err := f.s.PrepareEventsWait(t.Context(), workerCaller(), apicontract.EventsWaitArgs{})
		_, err = execute(t.Context(), call, err)
		return err
	default:
		call, err := f.s.PreparePromoteStatus(t.Context(), workerCaller(), apicontract.PromoteStatusArgs{ID: "pending"})
		_, err = execute(t.Context(), call, err)
		return err
	}
}
