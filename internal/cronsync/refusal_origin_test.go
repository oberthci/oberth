package cronsync_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/schedule"
)

func TestAdmissionRefusalKeepsItsOwnFileAndReason(t *testing.T) {
	for _, row := range []struct {
		stage string
		code  publicerr.Code
	}{
		{"admission", publicerr.InvalidInput}, {"admission", publicerr.NotFound}, {"validation", publicerr.InvalidInput}, {"validation", publicerr.NotFound},
	} {
		t.Run(row.stage+"_"+string(row.code), func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			failure := publicerr.New(row.code, "build file could not be prepared", "Fix .oberth/build.yaml and push to the default branch.", nil)
			failure.Detail = `file ".oberth/build.yaml": field "steps" has value []: add a step.`
			if row.stage == "admission" {
				f.admitErr = failure
			} else {
				f.validationErr = failure
			}
			f.sync()
			status, err := f.c.Status(t.Context(), f.repo, "nightly")
			want := "build file could not be prepared - Fix .oberth/build.yaml and push to the default branch.\n" + failure.Detail
			if err != nil || status.Error != "schedule cannot be prepared" || status.Detail != want || !status.SyncSuspended {
				t.Fatal(status, err)
			}
		})
	}
}
func TestJoinedAdmissionRefusalDetailsAreNotClassifiedAgain(t *testing.T) {
	f := newFixture(t)
	first := publicerr.New(publicerr.InvalidInput, "build step is invalid", "Fix .oberth/build.yaml.", nil)
	first.Detail = `file ".oberth/build.yaml": field "steps.a" has value []: add a step.`
	second := publicerr.New(publicerr.InvalidInput, "build image is invalid", "Fix .oberth/build.yaml.", nil)
	second.Detail = `file ".oberth/build.yaml": field "steps.b.image" has value "": name an image.`
	f.admitErr = errors.Join(first, second)
	f.sync()
	statuses, err := f.c.Snapshot(t.Context(), f.repo)
	want := "Several problems blocked this operation. - Review the detailed problems and retry.\n" + first.Detail + "\n" + second.Detail
	if err != nil || len(statuses) != 1 || statuses[0].Error != "schedule cannot be prepared" || statuses[0].Detail != want {
		t.Fatal(statuses, err)
	}
}
func TestUnclassifiedFailuresHaveCompletePublicMessages(t *testing.T) {
	for _, port := range []string{"admission", "git"} {
		t.Run(port, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			outage := errors.New("private dependency data")
			if port == "admission" {
				f.admitErr = outage
			} else {
				f.gitHook = func(_ context.Context, verb, _ string) error {
					if verb == "read" {
						return outage
					}
					return nil
				}
			}
			if err := f.c.SyncAll(t.Context()); !errors.Is(err, outage) {
				t.Fatal("transient failure did not remain pending", err)
			}
			status, err := f.c.Status(t.Context(), f.repo, "nightly")
			reason := "schedule admission is unavailable"
			remedy := "Restore Git, policy and store access, then retry the schedule update."
			if port == "git" {
				reason = "schedule Git is unavailable"
				remedy = "Restore repository access and retry the schedule update."
			}
			want := reason + " - " + remedy
			if err != nil || status.Error != reason || status.Detail != want || status.SyncSuspended {
				t.Fatal(status, err)
			}
		})
	}
}
func TestPermanentScheduleReadRefusalNamesItsFile(t *testing.T) {
	for _, wrap := range []func(error) error{func(err error) error { return err }, func(err error) error { return fmt.Errorf("wrapped: %w", err) }} {
		f := newFixture(t)
		f.sync()
		before := f.object(f.repo, "nightly")
		f.gitHook = func(_ context.Context, verb, _ string) error {
			if verb == "read" {
				return wrap(&gitport.AuthorRefusalError{Path: schedule.FileName, Code: "invalid_path"})
			}
			return nil
		}
		f.sync()
		status, err := f.c.Status(t.Context(), f.repo, "nightly")
		want := "schedule file is invalid - " + fileRemedy + "\nfile: field \"path\" has value \".oberth/schedule.yaml\": restore this file at the default-branch head or remove its declaration."
		if err != nil || status.Error != "schedule file is invalid" || status.Detail != want || !status.SyncSuspended || f.object(f.repo, "nightly").UID != before.UID {
			t.Fatal(status, err)
		}
	}
}
func TestMalformedScheduleDoesNotEchoUnrelatedDocumentValues(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "# unrelated-private-value\nschedules: [\n")
	f.sync()
	statuses, err := f.c.Snapshot(t.Context(), f.repo)
	want := "schedule file is not valid YAML - " + fileRemedy + "\nfile: field \"document\": yaml: line 2: did not find expected node content; fix YAML at the reported line."
	if err != nil || len(statuses) != 1 || statuses[0].Error != "schedule file is invalid" || statuses[0].Detail != want {
		t.Fatal(statuses, err)
	}
}
