package app

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"golang.org/x/mod/semver"
)

// ReleaseRuns reads the server's existing run history and immutable admissions.
type ReleaseRuns interface {
	Runs(context.Context, stateport.RunFilter) ([]stateport.Run, error)
	Run(context.Context, string) (stateport.Run, error)
}

// VerifiedReleaseFacts uses the ancestry decision's exact head and stored build
// result. Successful and in-flight releases define this repository’s version floor.
func VerifiedReleaseFacts(ctx context.Context, r Request, file string, runs ReleaseRuns) (admissioncontract.ReleaseFacts, error) {
	if r.Original != nil {
		return retainedReleaseFacts(ctx, r, file, runs)
	}
	facts := admissioncontract.ReleaseFacts{Version: strings.TrimPrefix(r.Ref.Name, "refs/tags/"), TestedCommit: r.Ref.CommitSHA, DefaultCommit: r.ReleaseHead.CommitSHA}
	if !r.Release || r.Trigger != model.TriggerTag || !workflowpolicy.IsReleaseVersion(facts.Version) {
		return facts, refusal("release_version", nil)
	}
	if !gitoid.Valid(facts.DefaultCommit) {
		return facts, refusal("release_head", nil)
	}
	if err := newerRelease(ctx, r.Repository, facts.Version, runs); err != nil {
		return facts, err
	}
	if err := passedBuild(ctx, r, runs); err != nil {
		return facts, err
	}
	facts.BuildStatus = string(stepmodel.RunPassed)
	return facts, nil
}

func newerRelease(ctx context.Context, repo, version string, runs ReleaseRuns) error {
	filter := stateport.RunFilter{Repositories: []string{repo}, Trigger: model.TriggerTag, Limit: stateport.RunPageLimit}
	for {
		rows, err := runs.Runs(ctx, filter)
		if err != nil {
			return err
		}
		for _, row := range rows {
			known := strings.TrimPrefix(row.Ref, "refs/tags/")
			active := row.Status == stepmodel.RunPassed || row.Status == stepmodel.RunQueued || row.Status == stepmodel.RunRunning
			if row.Release && active && workflowpolicy.IsReleaseVersion(known) && semver.Compare(version, known) <= 0 {
				return refusal("release_old", nil)
			}
			filter.Before = row.Sequence
		}
		if len(rows) < filter.Limit {
			return nil
		}
	}
}

func passedBuild(ctx context.Context, r Request, runs ReleaseRuns) error {
	filter := stateport.RunFilter{Repositories: []string{r.Repository}, CommitSHA: r.Ref.CommitSHA, Limit: stateport.RunPageLimit}
	for {
		rows, err := runs.Runs(ctx, filter)
		if err != nil {
			return err
		}
		for _, row := range rows {
			filter.Before = row.Sequence
			found, err := buildResult(ctx, row, runs)
			if found || err != nil {
				return err
			}
		}
		if len(rows) < filter.Limit {
			return refusal("release_build", nil)
		}
	}
}

func retainedReleaseFacts(ctx context.Context, r Request, file string, runs ReleaseRuns) (admissioncontract.ReleaseFacts, error) {
	old := *r.Original
	v := old.Admission
	if !sameReleaseRun(old, r, file) {
		return admissioncontract.ReleaseFacts{}, refusal("release_rerun", nil)
	}

	values := map[string]string{}
	for _, p := range v.Workflow.Spec.Arguments.Parameters {
		if admissioncontract.IsReleaseParameter(p.Name) && p.Value != nil {
			values[p.Name] = p.Value.String()
		}
	}
	facts := admissioncontract.ReleaseFacts{Version: values["version"], TestedCommit: values["release-tested-commit"], DefaultCommit: values["release-default-commit"], BuildStatus: values["release-build-status"]}
	if facts.Version != strings.TrimPrefix(v.Ref, "refs/tags/") ||
		!gitoid.Valid(facts.DefaultCommit) || facts.BuildStatus != string(stepmodel.RunPassed) {
		return facts, refusal("release_rerun", nil)
	}
	if err := newerRelease(ctx, r.Repository, facts.Version, runs); err != nil {
		return facts, err
	}
	return facts, nil
}

func buildResult(ctx context.Context, row stateport.Run, runs ReleaseRuns) (bool, error) {
	if row.Release || !row.HasAdmission {
		return false, nil
	}
	full, err := runs.Run(ctx, row.ID)
	if err != nil {
		return false, err
	}
	if full.Admission.SelectedFile != workflowpolicy.BuildFile {
		return false, nil
	}
	if full.Status != stepmodel.RunPassed || full.Origin.HasAssistance() {
		return true, refusal("release_build", nil)
	}
	return true, nil
}

func sameReleaseRun(old stateport.Run, r Request, file string) bool {
	v := old.Admission
	return old.Release && r.Release && v.Workflow != nil &&
		v.Repository == r.Repository && v.Ref == r.Ref.Name && v.ObjectSHA == r.Ref.ObjectSHA &&
		v.CommitSHA == r.Ref.CommitSHA && v.Trigger == r.Trigger && v.SelectedFile == file &&
		old.Origin.AssistRequested == r.Assist
}
