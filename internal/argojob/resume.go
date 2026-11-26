package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// ResumeMargin is the native TTL a resume needs left: Argo's TTL collector
// deletes without a precondition, so a resume near expiry could lose the
// Workflow while the fixed commit runs.
const ResumeMargin = time.Hour

const (
	ReasonResumeWindow   = "resume window ended; push for a full run"
	ReasonResumeStart    = "resume could not start"
	ReasonResumeChanged  = "pipeline changed; push for a full run"
	ReasonResumeUnsafe   = "step cannot resume; push for a full run"
	reasonResumeFinish   = "run is still finishing"
	reasonResumeTooLarge = "Workflow too large to resume"
	remedyResume         = "Push the fix with -o oberth.run=1 for a full unassisted run."
	remedyResumeWait     = "Wait until the run's logs are archived, then resume again."
)

// ResumeInput is one live read of the failed predecessor's Workflow and its
// Pods, the predecessor's sealed admission, and the successor's identity.
type ResumeInput struct {
	Predecessor nativebinding.RunBinding
	Sealed      admissioncontract.PreparedView
	Workflow    *wf.Workflow
	Pods        []*core.Pod
	RunID       string
	Commit      string
	Now         time.Time
}

// ResumePod is a Pod Argo's retry deletes; UID is empty when it is already gone.
type ResumePod = nativebinding.ResumePod
type ResumePlan = nativebinding.ResumePlan

// PlanResume reads only. Every refusal comes before any effect, including the
// size of the write, so no Pod is deleted for a write that cannot land.
func PlanResume(ctx context.Context, in ResumeInput) (ResumePlan, error) {
	if err := verifyWorkflow(in.Predecessor, in.Workflow); err != nil {
		return ResumePlan{}, err
	}
	w := in.Workflow.DeepCopy()
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return ResumePlan{}, resumeRefusal(ReasonResumeStart, "decompress: "+err.Error())
	}
	if err := resumeWindow(w, in.Pods, in.Now); err != nil {
		return ResumePlan{}, err
	}
	if err := sameLiveSpec(in.Sealed.Workflow, w); err != nil {
		return ResumePlan{}, err
	}
	retried, names, err := formulateRetry(ctx, w)
	if err != nil {
		return ResumePlan{}, err
	}
	carried, remaining, err := resumeRemaining(ctx, retried, in.Sealed.Plan)
	if err != nil {
		return ResumePlan{}, err
	}
	pods, err := resumePods(names, in.Pods, w.UID)
	if err != nil {
		return ResumePlan{}, err
	}
	secretSteps := resumeSecretSteps(in.Sealed.Plan, remaining)
	relabel(retried, in.RunID, in.Commit)
	if err := packer.CompressWorkflowIfNeeded(ctx, retried); err != nil {
		return ResumePlan{}, resumeRefusal(reasonResumeTooLarge, err.Error())
	}
	observed := []ResumePod{}
	for _, p := range in.Pods {
		observed = append(observed, ResumePod{Name: p.Name, UID: p.UID})
	}
	return ResumePlan{ObservedPods: observed, Workflow: retried, UID: w.UID, ResourceVersion: w.ResourceVersion, Pods: pods, Carried: carried, Remaining: remaining, SecretSteps: secretSteps}, nil
}

func resumeRefusal(reason, detail string) error {
	remedy := remedyResume
	if reason == reasonResumeFinish {
		remedy = remedyResumeWait
	}
	e := publicerr.New(publicerr.InvalidTransition, reason, remedy, nil)
	e.Detail = detail
	return e
}

// resumeWindow is P3 on the live objects: a failed, unstopped, unoffloaded
// Workflow whose record finalizer was released, whose Pods all ended, and
// with at least ResumeMargin of native TTL left.
func resumeWindow(w *wf.Workflow, pods []*core.Pod, now time.Time) error {
	if err := resumableWorkflow(w); err != nil {
		return err
	}
	for _, p := range pods {
		if p.Status.Phase != core.PodSucceeded && p.Status.Phase != core.PodFailed {
			return resumeRefusal(reasonResumeFinish, "Pod "+p.Name+" has not ended")
		}
	}
	if ttl, ok := workflowTTL(w); ok {
		left := w.Status.FinishedAt.Add(ttl).Sub(now)
		if w.Status.FinishedAt.IsZero() || left < ResumeMargin {
			return resumeRefusal(ReasonResumeWindow, fmt.Sprintf("native TTL leaves %s", left.Truncate(time.Second)))
		}
	}
	return nil
}

func resumableWorkflow(w *wf.Workflow) error {
	switch {
	case w.DeletionTimestamp != nil:
		return resumeRefusal(ReasonResumeStart, "Workflow is being deleted")
	case w.Status.Phase != wf.WorkflowFailed && w.Status.Phase != wf.WorkflowError:
		return resumeRefusal(ReasonResumeStart, "Workflow phase "+string(w.Status.Phase))
	case w.Spec.Shutdown != "":
		return resumeRefusal(ReasonResumeStart, "Workflow was stopped")
	case w.Status.OffloadNodeStatusVersion != "":
		return resumeRefusal(ReasonResumeStart, "Workflow nodes are offloaded")
	case slices.Contains(w.Finalizers, nativebinding.RecordFinalizer):
		return resumeRefusal(reasonResumeFinish, "run record finalizer is still held")
	case workspaceDeleted(w):
		return resumeRefusal(ReasonResumeChanged, "workspace volumes were deleted at completion")
	}
	return nil
}

func workspaceDeleted(w *wf.Workflow) bool {
	gc := w.Spec.VolumeClaimGC
	return len(w.Spec.VolumeClaimTemplates) > 0 && gc != nil && gc.Strategy == wf.VolumeClaimGCOnCompletion
}

// workflowTTL mirrors the TTL Argo's collector applies (gccontroller ttl).
func workflowTTL(w *wf.Workflow) (time.Duration, bool) {
	s := w.GetTTLStrategy()
	switch {
	case s == nil:
		return 0, false
	case w.Status.Failed() && s.SecondsAfterFailure != nil:
		return time.Duration(*s.SecondsAfterFailure) * time.Second, true
	case w.Status.Phase.Completed() && s.SecondsAfterCompletion != nil:
		return time.Duration(*s.SecondsAfterCompletion) * time.Second, true
	}
	return 0, false
}

// sameLiveSpec is P5 on the live object: the native spec must still be the
// sealed one. Collection's PodGC replacement and TTL are part of the sealed
// spec; a shutdown already refused in resumeWindow.
func sameLiveSpec(sealed, live *wf.Workflow) error {
	if sealed == nil {
		return resumeRefusal(ReasonResumeChanged, "sealed Workflow is missing")
	}
	expected := sealed.DeepCopy()
	prepareCollection(expected)
	want, errWant := json.Marshal(expected.Spec)
	got, errGot := json.Marshal(live.Spec)
	if errWant != nil || errGot != nil || string(want) != string(got) {
		return resumeRefusal(ReasonResumeChanged, "live Workflow spec differs from the sealed spec")
	}
	return nil
}

// formulateRetry is the product's only native retry call. Argo panics on a
// decompress failure inside it; the panic refuses instead of ending the server.
func formulateRetry(ctx context.Context, w *wf.Workflow) (out *wf.Workflow, pods []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, pods, err = nil, nil, resumeRefusal(ReasonResumeStart, fmt.Sprint("native retry: ", r))
		}
	}()
	ctx = logging.WithLogger(ctx, retryLogger{})
	out, pods, err = argoutil.FormulateRetryWorkflow(ctx, w.DeepCopy(), false, "", nil)
	if err != nil {
		return nil, nil, resumeRefusal(ReasonResumeStart, "native retry: "+err.Error())
	}
	return out, pods, nil
}

// retryLogger discards Argo's retry logs without Argo's logger construction,
// whose init-log replay races with concurrent construction elsewhere. A
// panic or fatal log panics, and formulateRetry's recover refuses it.
type retryLogger struct{ abort bool }

func (l retryLogger) WithFields(logging.Fields) logging.Logger { return l }
func (l retryLogger) WithField(string, any) logging.Logger     { return l }
func (l retryLogger) WithError(error) logging.Logger           { return l }
func (retryLogger) WithPanic() logging.Logger                  { return retryLogger{abort: true} }
func (retryLogger) WithFatal() logging.Logger                  { return retryLogger{abort: true} }
func (l retryLogger) Debug(_ context.Context, msg string)      { l.log(msg) }
func (l retryLogger) Info(_ context.Context, msg string)       { l.log(msg) }
func (l retryLogger) Warn(_ context.Context, msg string)       { l.log(msg) }
func (l retryLogger) Error(_ context.Context, msg string)      { l.log(msg) }
func (retryLogger) Level() logging.Level                       { return logging.Error }
func (l retryLogger) NewBackgroundContext() context.Context {
	return logging.WithLogger(context.Background(), l)
}
func (l retryLogger) InContext(ctx context.Context) (context.Context, logging.Logger) {
	return logging.WithLogger(ctx, l), l
}
func (l retryLogger) log(msg string) {
	if l.abort {
		panic(msg)
	}
}

// resumeRemaining is P6 over the Workflow Argo's retry returns, never over a
// record: every plan entry not passed there runs again on the fixed commit
// and must have no output parameters. Workflow-level hooks are
// evaluated again and belong to the remaining set.
func resumeRemaining(ctx context.Context, retried *wf.Workflow, plan admissioncontract.Plan) ([]string, []string, error) {
	o, err := Project(ctx, Projection{Plan: plan, Workflow: retried})
	if err != nil {
		return nil, nil, resumeRefusal(ReasonResumeStart, "projection: "+err.Error())
	}
	entries := make(map[string]admissioncontract.PlanEntry, len(plan.Entries))
	for _, e := range plan.Entries {
		entries[e.Path] = e
	}

	carried, remaining := []string{}, []string{}
	for _, s := range o.Steps {
		e, ok := entries[s.Path]
		if s.Status == stepmodel.StepPassed && !resumeWorkflowHook(s.Path) && ok {
			carried = append(carried, s.Path)
			continue
		}
		remaining = append(remaining, s.Path)
		if why := unsafeEntry(e, ok, retried); why != "" {
			return nil, nil, resumeRefusal(ReasonResumeUnsafe, stepLabel(s)+" "+why)
		}
	}
	return carried, remaining, nil
}

func resumeWorkflowHook(path string) bool {
	return path == "onExit" || strings.HasPrefix(path, "onExit.") || strings.HasPrefix(path, "hooks.")
}

// unsafeEntry refuses unresolved entries and remaining output-parameter producers.
func unsafeEntry(e admissioncontract.PlanEntry, planned bool, w *wf.Workflow) string {
	if e.OutputParameters {
		return "has output parameters"
	}
	if !planned || e.Dynamic {
		return "eligibility unresolved"
	}
	for _, t := range w.Spec.Templates {
		if t.Name == e.Template && len(t.Outputs.Parameters) > 0 {
			return "has output parameters"
		}
	}
	return ""
}

func stepLabel(s stepmodel.RunStep) string {
	if s.Name != "" {
		return s.Name
	}
	return s.Path
}

// resumePods names the Pods by Argo's retry formula, with the UID of each one
// still present; a present Pod controlled by another object refuses.
func resumePods(names []string, live []*core.Pod, workflow types.UID) ([]ResumePod, error) {
	byName := make(map[string]*core.Pod, len(live))
	for _, p := range live {
		byName[p.Name] = p
	}
	out := make([]ResumePod, 0, len(names))
	for _, name := range slices.Sorted(slices.Values(names)) {
		p := byName[name]
		if p == nil {
			out = append(out, ResumePod{Name: name})
			continue
		}
		owner := meta.GetControllerOf(p)
		if owner == nil || owner.UID != workflow {
			return nil, ownership(fmt.Errorf("pod %s is not controlled by the Workflow", name))
		}
		out = append(out, ResumePod{Name: name, UID: p.UID})
	}
	return out, nil
}

// relabel moves the routing labels to the successor; UID, name, ref, trigger,
// repository and spec identity stay the predecessor's.
func relabel(w *wf.Workflow, runID, commit string) {
	w.Labels[nativebinding.RunLabel] = runID
	w.Labels[nativebinding.CommitLabel] = commit
	w.Annotations[nativebinding.RunIDAnnotation] = runID
	if !slices.Contains(w.Finalizers, nativebinding.RecordFinalizer) {
		w.Finalizers = append(w.Finalizers, nativebinding.RecordFinalizer)
	}
}

func resumeSecretSteps(plan admissioncontract.Plan, remaining []string) []string {
	names := []string{}
	for _, entry := range plan.Entries {
		if entry.SecretRoute && slices.Contains(remaining, entry.Path) {
			names = append(names, entry.Name)
		}
	}
	return names
}
