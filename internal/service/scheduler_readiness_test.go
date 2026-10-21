package service

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerReadinessOnlyAfterRecoveryAndUntilStop(t *testing.T) {
	fixture := newRecoveryFixture(t)
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	if scheduler.Recovered() {
		t.Fatal("ready before recovery")
	}
	recovered := make(chan struct{})
	scheduler.onStartupRecovered = func() { close(recovered) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-recovered:
	case <-time.After(time.Second):
		t.Fatal("recovery did not finish")
	}
	if !scheduler.Recovered() {
		t.Fatal("completed recovery not visible to readiness")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	if scheduler.Recovered() {
		t.Fatal("stopped scheduler remained ready")
	}
}
