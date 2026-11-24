package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestBuildEvidencePagesPastOptionalRunsAndStopsAtBound(t *testing.T) {
	for _, count := range []int{201, 400} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			testBuildEvidencePage(t, count)
		})
	}
}

func testBuildEvidencePage(t *testing.T, count int) {
	t.Helper()
	f := setup(t)
	build := f.seed(stepmodel.RunPassed)
	for i := range count {
		optional := build
		optional.ID, optional.Sequence = fmt.Sprintf("%032x", i+1000), 0
		optional.Origin.RequestedFile = ".oberth/walk.yaml"
		optional.Admission.SelectedFile = optional.Origin.RequestedFile
		f.store.native.SeedRun(optional)
	}
	before := f.store.Calls()["Runs"]
	call, err := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land tested work"})
	if got := f.store.Calls()["Runs"] - before; got != 2 {
		t.Fatal("build evidence paging is missing or unbounded", got)
	}
	if count == 400 {
		if err == nil || !strings.Contains(err.Error(), "Build evidence scan limit reached.") {
			t.Fatal("old build evidence bypassed the scan bound", err)
		}
		return
	}
	if err != nil {
		t.Fatal("second page lost passing build evidence", err)
	}
	out, err := call(t.Context())
	if err != nil || out.TestedRunID != build.ID {
		t.Fatal("paged evidence selected an optional workflow", out, err)
	}
}
