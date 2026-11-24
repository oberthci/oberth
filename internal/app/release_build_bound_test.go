package app_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type releaseScanCounter struct {
	releaseHistory
	pages, reads int
	filters      []stateport.RunFilter
}

func (h *releaseScanCounter) Runs(ctx context.Context, f stateport.RunFilter) ([]stateport.Run, error) {
	h.pages++
	h.filters = append(h.filters, f)
	return h.releaseHistory.Runs(ctx, f)
}

func (h *releaseScanCounter) Run(ctx context.Context, id string) (stateport.Run, error) {
	h.reads++
	return h.releaseHistory.Run(ctx, id)
}

func TestReleaseBuildScanBoundsLargeCommitHistory(t *testing.T) {
	for _, count := range []int{201, 400, 50000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			checkReleaseBuildScan(t, count)
		})
	}
}

func checkReleaseBuildScan(t *testing.T, count int) {
	t.Helper()
	r, build := releaseRequest(), buildHistory()[0]
	h := &releaseScanCounter{}
	for i := count; i > 0; i-- {
		row := build.Clone()
		row.ID, row.Sequence = fmt.Sprint(i), int64(i+1)
		row.Origin.RequestedFile = ".oberth/walk.yaml"
		row.Admission.SelectedFile = row.Origin.RequestedFile
		h.releaseHistory = append(h.releaseHistory, row)
	}
	h.releaseHistory = append(h.releaseHistory, build)
	_, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
	if h.pages != 3 {
		t.Fatal("release build scan exceeded two pages plus one version-floor query", h.pages)
	}
	for _, filter := range h.filters[1:] {
		filter.Before = 0
		want := stateport.RunFilter{Repositories: []string{r.Repository}, CommitSHA: r.Ref.CommitSHA, Limit: stateport.RunPageLimit}
		if !reflect.DeepEqual(filter, want) {
			t.Fatal("build scan lost its indexed exact commit scope or page limit", filter)
		}
	}
	if count < 400 {
		if err != nil || h.reads != 1 {
			t.Fatal("second page lost passing build", err, h.reads)
		}
	} else {
		expectRefusal(t, err, "release_limit")
		if got := err.(interface{ Remedy() string }).Remedy(); got != "Run .oberth/build.yaml again on the tagged commit, then push the tag again." {
			t.Fatal("release scan limit lost its exact next action", got)
		}
		if h.reads != 0 {
			t.Fatal("optional runs loaded full admissions", h.reads)
		}
	}
	t.Logf("history=%d list queries=%d full admissions=%d", count+1, h.pages, h.reads)
}
