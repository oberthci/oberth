package app_test

import (
	"context"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestScheduleAdmitsLockPlacementAdvisory(t *testing.T) {
	f := newFixture(t)
	read := f.source.Read
	f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
		file, err := read(ctx, r, path)
		w := workflow()
		w.Spec.Synchronization = &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: request().Repository + "/publish"}}}
		file.Bytes = document(w)
		return file, err
	}
	r := request()
	r.ScheduleEntry = "nightly"
	prepared, err := f.new(t).PrepareSchedule(t.Context(), r)
	if err != nil || prepared == nil {
		t.Fatal("advisory blocked schedule admission", err)
	}
	findings := prepared.View().Findings
	if len(findings) != 1 || !findings[0].Advisory || findings[0].Code != "compound_lock" || f.creates.Load() != 0 {
		t.Fatal("schedule lost advisory or started work", findings)
	}
}
