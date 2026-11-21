package app_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type releaseHistory []stateport.Run

func (h releaseHistory) Run(ctx context.Context, id string) (stateport.Run, error) {
	for _, r := range h {
		if r.ID == id {
			return r.Clone(), ctx.Err()
		}
	}
	return stateport.Run{}, errors.New("run missing")
}
func (h releaseHistory) Runs(ctx context.Context, f stateport.RunFilter) ([]stateport.Run, error) {
	var rows []stateport.Run
	for _, r := range h {
		if !releaseHistoryMatch(r, f) {
			continue
		}
		if f.Before > 0 && r.Sequence >= f.Before {
			continue
		}
		rows = append(rows, r.Clone())
		if len(rows) == f.Limit {
			break
		}
	}
	return rows, ctx.Err()
}
func releaseRequest() app.Request {
	r := request()
	r.Release, r.Trigger = true, model.TriggerTag
	r.Ref.Name = "refs/tags/v1.2.3"
	r.ReleaseHead = sourcecontract.Ref{Name: "refs/heads/main", CommitSHA: strings.Repeat("c", 40), ObjectSHA: strings.Repeat("c", 40)}
	return r
}
func buildHistory() releaseHistory {
	r := releaseRequest()
	return releaseHistory{{RunSummary: stepmodel.RunSummary{ID: "build", Repo: r.Repository, Ref: "refs/heads/main", CommitSHA: &r.Ref.CommitSHA, Status: stepmodel.RunPassed}, Sequence: 1, HasAdmission: true, Admission: admissioncontract.PreparedView{SelectedFile: workflowpolicy.BuildFile}}}
}
func TestReleaseFactsComeFromVerifiedTagHeadAndBuild(t *testing.T) {
	r := releaseRequest()
	facts, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, buildHistory())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"version": "v1.2.3", "release-tested-commit": r.Ref.CommitSHA, "release-default-commit": r.ReleaseHead.CommitSHA, "release-build-status": "passed"}
	for name, value := range want {
		t.Run(name, func(t *testing.T) {
			if facts.Parameters()[name] != value {
				t.Fatal(facts)
			}
		})
	}
}
func TestReleaseFactsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*app.Request, *releaseHistory)
	}{
		{"invalid-version", "release_version", func(r *app.Request, _ *releaseHistory) { r.Ref.Name = "refs/tags/v1.2.3junk" }},
		{"non-release", "release_version", func(r *app.Request, _ *releaseHistory) { r.Release = false }},
		{"branch", "release_version", func(r *app.Request, _ *releaseHistory) { r.Trigger = model.TriggerBranch }},
		{"missing-head", "release_head", func(r *app.Request, _ *releaseHistory) { r.ReleaseHead = sourcecontract.Ref{} }},
		{"failed-build", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].Status = stepmodel.RunFailed }},
		{"running-build", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].Status = stepmodel.RunRunning }},
		{"missing-build", "release_build", func(_ *app.Request, h *releaseHistory) { *h = nil }},
		{"other-commit", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].CommitSHA = new(strings.Repeat("d", 40)) }},
		{"other-repository", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].Repo = "origin/team/other" }},
		{"other-workflow", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].Admission.SelectedFile = ".oberth/other.yaml" }},
		{"assisted-build", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].Origin.Assisted = true }},
		{"no-admission", "release_build", func(_ *app.Request, h *releaseHistory) { (*h)[0].HasAdmission = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, history := releaseRequest(), buildHistory()
			tc.change(&r, &history)
			_, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, history)
			expectRefusal(t, err, tc.code)
		})
	}
}
func TestReleaseVersionUsesHighestSuccessfulRepositoryRelease(t *testing.T) {
	for _, tc := range []struct {
		tag                   string
		passed, release, want bool
	}{
		{"v1.2.2", true, true, true}, {"v1.2.3", true, true, false}, {"v2.0.0", true, true, false},
		{"v1.2.3-rc.1", true, true, true}, {"v1.2.3+build", true, true, true},
		{"v9.0.0", false, true, true}, {"v9.0.0", true, false, true}, {"ordinary", true, true, true},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			r, h := releaseRequest(), buildHistory()
			old := stateport.Run{RunSummary: stepmodel.RunSummary{ID: "release", Repo: r.Repository, Ref: "refs/tags/" + tc.tag, Status: stepmodel.RunFailed}, Origin: model.RunOrigin{RunCause: model.RunCause{Trigger: model.TriggerTag}}, Sequence: 2, Release: tc.release}
			if tc.passed {
				old.Status = stepmodel.RunPassed
			}
			h = append(releaseHistory{old}, h...)
			_, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
			if (err == nil) != tc.want {
				t.Fatal(err)
			}
		})
	}
}
func TestReleaseRerunRetainsFactsAndRefusesChangedIdentity(t *testing.T) {
	r, h := releaseRequest(), buildHistory()
	facts, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
	if err != nil {
		t.Fatal(err)
	}
	original := stateport.Run{RunSummary: stepmodel.RunSummary{ID: "original", Repo: r.Repository}, Release: true, HasAdmission: true, Admission: admissioncontract.PreparedView{Workflow: &wf.Workflow{}, Repository: r.Repository, Ref: r.Ref.Name, ObjectSHA: r.Ref.ObjectSHA, CommitSHA: r.Ref.CommitSHA, Trigger: r.Trigger, SelectedFile: workflowpolicy.ReleaseFile}}
	for name, value := range facts.Parameters() {
		original.Admission.Workflow.Spec.Arguments.Parameters = append(original.Admission.Workflow.Spec.Arguments.Parameters, wf.Parameter{Name: name, Value: wf.AnyStringPtr(value)})
	}
	r.Original = &original
	r.ReleaseHead.CommitSHA = strings.Repeat("d", 40)
	h = releaseHistory{original}
	got, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
	if err != nil || !reflect.DeepEqual(facts, got) {
		t.Fatal(got, err)
	}
	for _, field := range []string{"tag", "object", "commit", "repo", "trigger", "assist", "file", "not-release", "missing-facts"} {
		t.Run(field, func(t *testing.T) {
			changed, history, file := r, releaseHistory{original.Clone()}, workflowpolicy.ReleaseFile
			changed, history, file = changedReleaseRerun(field, changed, history, file)
			changed.Original = &history[0]
			_, err := app.VerifiedReleaseFacts(t.Context(), changed, file, history)
			expectRefusal(t, err, "release_rerun")
		})
	}
}

func releaseHistoryMatch(r stateport.Run, f stateport.RunFilter) bool {
	if r.Repo != f.Repositories[0] || f.CommitSHA != "" && (r.CommitSHA == nil || *r.CommitSHA != f.CommitSHA) {
		return false
	}
	return (f.Trigger == "" || r.Origin.Trigger == f.Trigger) && (f.Status == "" || r.Status == f.Status)
}

func changedReleaseRerun(field string, changed app.Request, history releaseHistory, file string) (app.Request, releaseHistory, string) {
	switch field {
	case "tag":
		changed.Ref.Name = "refs/tags/v1.2.4"
	case "object":
		changed.Ref.ObjectSHA = strings.Repeat("e", 40)
	case "commit":
		changed.Ref.CommitSHA = strings.Repeat("e", 40)
	case "repo":
		changed.Repository = "origin/team/other"
	case "trigger":
		changed.Trigger = model.TriggerBranch
	case "assist":
		changed.Assist = true
	case "file":
		file = ".oberth/other.yaml"
	case "not-release":
		history[0].Release = false
	case "missing-facts":
		history[0].Admission.Workflow.Spec.Arguments.Parameters = nil
	}
	return changed, history, file
}

func TestReleaseBuildUsesLatestResultAndVersionFloorCrossesPages(t *testing.T) {
	r, h := releaseRequest(), buildHistory()
	failed := h[0].Clone()
	failed.ID, failed.Sequence, failed.Status = "failed-newer", 2, stepmodel.RunFailed
	_, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, append(releaseHistory{failed}, h...))
	expectRefusal(t, err, "release_build")
	for i := stateport.RunPageLimit + 2; i > 1; i-- {
		h = append(releaseHistory{{RunSummary: stepmodel.RunSummary{ID: "unrelated", Repo: r.Repository, Ref: "refs/tags/v1.0.0", Status: stepmodel.RunPassed}, Sequence: int64(i), Origin: model.RunOrigin{RunCause: model.RunCause{Trigger: model.TriggerTag}}, Release: true}}, h...)
	}
	// Keep the server's descending history order, with the higher version beyond page one.
	for i := range h {
		h[i].Sequence = int64(len(h) - i)
	}
	h[len(h)-1] = stateport.Run{RunSummary: stepmodel.RunSummary{Repo: r.Repository, Ref: "refs/tags/v9.0.0", Status: stepmodel.RunPassed}, Origin: model.RunOrigin{RunCause: model.RunCause{Trigger: model.TriggerTag}}, Sequence: 1, Release: true}
	_, err = app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
	expectRefusal(t, err, "release_old")
}

func TestReleaseVersionFloorIncludesInFlightRuns(t *testing.T) {
	for _, status := range []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning, stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled} {
		for _, version := range []string{"v1.2.2", "v1.2.3", "v2.0.0"} {
			t.Run(string(status)+"/"+version, func(t *testing.T) {
				r, h := releaseRequest(), buildHistory()
				old := stateport.Run{RunSummary: stepmodel.RunSummary{ID: "other-release", Repo: r.Repository, Ref: "refs/tags/" + version, Status: status}, Origin: model.RunOrigin{RunCause: model.RunCause{Trigger: model.TriggerTag}}, Sequence: 2, Release: true}
				_, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, append(releaseHistory{old}, h...))
				blocked := version != "v1.2.2" && (status == stepmodel.RunQueued || status == stepmodel.RunRunning || status == stepmodel.RunPassed)
				if blocked {
					expectRefusal(t, err, "release_old")
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReleaseRerunCannotPublishBelowCurrentVersionFloor(t *testing.T) {
	r, h := releaseRequest(), buildHistory()
	facts, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
	if err != nil {
		t.Fatal(err)
	}
	old := stateport.Run{RunSummary: stepmodel.RunSummary{ID: "failed-release", Repo: r.Repository, Ref: r.Ref.Name, Status: stepmodel.RunFailed}, Origin: model.RunOrigin{RunCause: model.RunCause{Trigger: model.TriggerTag}}, Release: true, Sequence: 1, HasAdmission: true, Admission: admissioncontract.PreparedView{Workflow: &wf.Workflow{}, Repository: r.Repository, Ref: r.Ref.Name, ObjectSHA: r.Ref.ObjectSHA, CommitSHA: r.Ref.CommitSHA, Trigger: r.Trigger, SelectedFile: workflowpolicy.ReleaseFile}}
	for name, value := range facts.Parameters() {
		old.Admission.Workflow.Spec.Arguments.Parameters = append(old.Admission.Workflow.Spec.Arguments.Parameters, wf.Parameter{Name: name, Value: wf.AnyStringPtr(value)})
	}
	r.Original = &old
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunRunning, stepmodel.RunQueued, stepmodel.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			newer := stateport.Run{RunSummary: stepmodel.RunSummary{ID: "newer-release", Repo: r.Repository, Ref: "refs/tags/v1.3.0", Status: status}, Origin: model.RunOrigin{RunCause: model.RunCause{Trigger: model.TriggerTag}}, Release: true, Sequence: 2}
			_, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, releaseHistory{newer, old})
			if status == stepmodel.RunFailed {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				expectRefusal(t, err, "release_old")
			}
		})
	}
}

type releaseReadCounter struct {
	releaseHistory
	reads int
}

func (h *releaseReadCounter) Run(ctx context.Context, id string) (stateport.Run, error) {
	h.reads++
	return h.releaseHistory.Run(ctx, id)
}

func TestReleaseRerunUsesLoadedOriginalWithoutAnotherRead(t *testing.T) {
	r := releaseRequest()
	original := stateport.Run{RunSummary: stepmodel.RunSummary{ID: "original", Repo: r.Repository, Status: stepmodel.RunFailed}, Release: true, Admission: admissioncontract.PreparedView{Workflow: &wf.Workflow{}, Repository: r.Repository, Ref: r.Ref.Name, ObjectSHA: r.Ref.ObjectSHA, CommitSHA: r.Ref.CommitSHA, Trigger: r.Trigger, SelectedFile: workflowpolicy.ReleaseFile}}
	facts := admissioncontract.ReleaseFacts{Version: "v1.2.3", TestedCommit: r.Ref.CommitSHA, DefaultCommit: r.ReleaseHead.CommitSHA, BuildStatus: "passed"}
	for name, value := range facts.Parameters() {
		original.Admission.Workflow.Spec.Arguments.Parameters = append(original.Admission.Workflow.Spec.Arguments.Parameters, wf.Parameter{Name: name, Value: wf.AnyStringPtr(value)})
	}
	r.Original = &original
	history := &releaseReadCounter{releaseHistory: releaseHistory{original}}
	got, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, history)
	if err != nil || got != facts || history.reads != 0 {
		t.Fatal("rerun reread the original", got, history.reads, err)
	}
}
