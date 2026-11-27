package service_test

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestLogScanWaitReauthorizesBeforeReadAndDelivery(t *testing.T) {
	for _, change := range []string{"revoke", "hide-repository"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { checkLogScanAuthority(t, change) })
		})
	}
}

func checkLogScanAuthority(t *testing.T, change string) {
	t.Helper()
	f := setup(t)
	f.seed("running")
	release := make(chan struct{})
	reads := 0
	f.logs.ReadFunc = func(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		reads++
		<-release
		return apicontract.LogResult{Content: "private log result"}, nil
	}
	caller := apicontract.Caller{Actor: caller(), CredentialID: "observer"}
	call, err := f.s.PrepareRunLogs(t.Context(), caller, apicontract.RunLogsArgs{ID: runID, Pattern: new("private")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan logDelivery, 2)
	for range 2 {
		go func() { result, err := call(t.Context()); done <- logDelivery{result, err} }()
		synctest.Wait()
	}
	if reads != 1 {
		t.Fatalf("expected one active and one queued scan, got %d reads", reads)
	}
	changeDeliveryAuthority(t, f, t.Context(), change)
	close(release)
	synctest.Wait()
	for range 2 {
		got := <-done
		if got.err == nil || !reflect.DeepEqual(got.result, apicontract.LogResult{}) {
			t.Fatalf("scan wait delivered unauthorized log snapshot: %+v, err=%v", got.result, got.err)
		}
	}
	if reads != 1 {
		t.Fatalf("queued scan read after authority changed: %d reads", reads)
	}
}

type logDelivery struct {
	result apicontract.LogResult
	err    error
}
