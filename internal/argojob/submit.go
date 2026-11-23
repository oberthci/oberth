package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	core "k8s.io/api/core/v1"
	"strconv"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Create posts once, binds in the durable store and starts observation.
func (e *Executor) Create(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
	v := p.View()
	r, err := e.config.Runs.Run(ctx, v.RunID)
	if err != nil {
		return nativebinding.RunBinding{}, err
	}
	name := nativebinding.WorkflowName(v.RunID)
	w := v.Workflow.DeepCopy()
	w.Name, w.GenerateName = name, ""
	obligations, err := e.config.Runs.Obligations(ctx, v.RunID)
	if err != nil {
		return nativebinding.RunBinding{}, err
	}
	for _, obligation := range obligations {
		if obligation.Kind == "stop" {
			w.Spec.Shutdown = wf.ShutdownStrategyTerminate
		}
	}
	stampSubmission(w, v, r.RepositoryID)
	instanceid.NewService(e.config.InstanceID).Label(w)
	created, err := e.config.Workflows.Create(ctx, w, meta.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) && !uncertain(err) {
			return nativebinding.RunBinding{}, createRefusal(err)
		}
		created, err = e.config.Workflows.Get(ctx, name, meta.GetOptions{})
		if err != nil {
			return nativebinding.RunBinding{}, unavailable(err)
		}
	}
	b := runBinding(v, r.RepositoryID, nativebinding.Object{Namespace: w.Namespace, Name: name, UID: string(created.UID)})
	if err = verifyWorkflow(b, created); err != nil {
		return nativebinding.RunBinding{}, err
	}
	if err = e.config.Runs.BindWorkflow(ctx, stateport.Action{Actor: r.Origin.Actor}, b); err != nil {
		return b, err
	}
	e.track(b, v.Plan, trackingSeed{})
	return b, nil
}

// Find settles unbound recovery by its fixed native name without another POST.
func (e *Executor) Find(ctx context.Context, r stateport.Run) (nativebinding.RunBinding, bool, error) {
	name := nativebinding.WorkflowName(r.ID)
	w, err := e.config.Workflows.Get(ctx, name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nativebinding.RunBinding{}, false, nil
	}
	if err != nil {
		return nativebinding.RunBinding{}, false, unavailable(err)
	}
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: r.Admission.Identity.Namespace, Name: name, UID: string(w.UID)})
	if err = verifyWorkflow(b, w); err != nil {
		return b, false, err
	}
	if err = e.config.Runs.BindWorkflow(ctx, stateport.Action{Actor: r.Origin.Actor}, b); err != nil {
		return b, false, err
	}
	e.track(b, r.Admission.Plan, trackingSeed{})
	return b, true, nil
}
func runBinding(v admissioncontract.PreparedView, repo int64, o nativebinding.Object) nativebinding.RunBinding {
	return nativebinding.RunBinding{Workflow: o, RepositoryID: repo, RunID: v.RunID, Revision: sourcecontract.Revision{Repository: v.Repository, Commit: v.CommitSHA}, Ref: v.Ref, Trigger: string(v.Trigger), Identity: v.Digest, SpecSHA256: v.SpecDigest}
}
func uncertain(err error) bool {
	return !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) && !authorizationRefusal(err) && !apierrors.IsNotFound(err) && !apierrors.IsMethodNotSupported(err) && !apierrors.IsRequestEntityTooLargeError(err) && !apierrors.IsNotAcceptable(err) && !apierrors.IsUnsupportedMediaType(err)
}

func authorizationRefusal(err error) bool {
	if apierrors.IsUnauthorized(err) {
		return true
	}
	if !apierrors.IsForbidden(err) || apierrors.HasStatusCause(err, core.NamespaceTerminatingCause) {
		return false
	}
	if apierrors.HasStatusCause(err, meta.CauseTypeForbidden) {
		return true
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	message := status.Status().Message
	return strings.Contains(message, "cannot create resource") || strings.Contains(message, "ValidatingAdmissionPolicy ") || strings.HasPrefix(message, "admission webhook ") && strings.Contains(message, " denied the request")
}

func createRefusal(cause error) error {
	code := publicerr.InvalidInput
	if authorizationRefusal(cause) {
		code = publicerr.Forbidden
	}
	err := publicerr.New(code, "Workflow creation was refused", "Correct the Workflow or restore cluster access, then submit again.", cause)
	err.Detail = cause.Error()
	return err
}

// Adopt reads the bound object; only live NotFound establishes absence.
func (e *Executor) Adopt(ctx context.Context, b nativebinding.RunBinding) (*wf.Workflow, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if e.config.Cache != nil {
		if w, ok := e.config.Cache.Workflow(b.Workflow.Name); ok {
			return w, e.verifyObserved(ctx, b, w)
		}
	}
	w, err := e.config.Workflows.Get(ctx, b.Workflow.Name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, absent(err)
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return w, e.verifyObserved(ctx, b, w)
}
func verifyWorkflow(b nativebinding.RunBinding, w *wf.Workflow) error {
	if w == nil {
		return ownership(nil)
	}
	got := b
	got.Workflow = nativebinding.Object{Namespace: w.Namespace, Name: w.Name, UID: string(w.UID)}
	got.RunID, got.Ref, got.SpecSHA256 = w.Annotations[nativebinding.RunIDAnnotation], w.Annotations[nativebinding.RefAnnotation], w.Annotations[nativebinding.IdentityAnnotation]
	got.RepositoryID, _ = strconv.ParseInt(w.Labels[nativebinding.RepositoryLabel], 10, 64)
	got.Revision.Commit, got.Trigger = w.Labels[nativebinding.CommitLabel], w.Labels[nativebinding.TriggerLabel]
	if w.Labels[nativebinding.EngineLabel] != nativebinding.EngineValue || w.Labels[nativebinding.RunLabel] != b.RunID {
		return ownership(nil)
	}
	if err := b.Verify(got); err != nil {
		return ownership(err)
	}
	return nil
}
func (e *Executor) Stop(ctx context.Context, b nativebinding.RunBinding) error {
	return e.cancel(ctx, b, wf.ShutdownStrategyStop)
}
func (e *Executor) Terminate(ctx context.Context, b nativebinding.RunBinding) error {
	return e.cancel(ctx, b, wf.ShutdownStrategyTerminate)
}
func (e *Executor) cancel(ctx context.Context, b nativebinding.RunBinding, mode wf.ShutdownStrategy) error {
	w, err := e.config.Workflows.Get(ctx, b.Workflow.Name, meta.GetOptions{})
	if err != nil {
		return unavailable(err)
	}
	if err = e.verifyObserved(ctx, b, w); err != nil {
		return err
	}
	if terminalWorkflow(w) {
		return nil
	}
	if w.Spec.Shutdown == wf.ShutdownStrategyTerminate || w.Spec.Shutdown == mode {
		return nil
	}
	body, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": b.Workflow.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": w.ResourceVersion}, {"op": "add", "path": "/spec/shutdown", "value": mode}})
	_, err = e.config.Workflows.Patch(ctx, PatchRequest{Name: w.Name, Body: body})
	return err
}

// unpacked owns one copy; the native synchronous decompressor is checked for
// cancellation on both sides, rather than advertised as interruptible I/O.
func (e *Executor) unpacked(ctx context.Context, b nativebinding.RunBinding) (*wf.Workflow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w, err := e.Adopt(ctx, b)
	if err != nil {
		return nil, err
	}
	w = w.DeepCopy()
	if err = packer.DecompressWorkflow(ctx, w); err != nil {
		return nil, err
	}
	return w, ctx.Err()
}
