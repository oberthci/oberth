package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

var dataVolumeResource = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
var pvcResource = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

// A deleting target keeps its resource Pod at the token barrier. The next
// controller tick retries observation; slow guest/storage shutdown is not a
// fatal API error and must not mark the delete step successful prematurely.
var errResourceDeletePending = errors.New("argojob: resource deletion is pending")

func workflowOwnsResource(wf *wfv1.Workflow, object *unstructured.Unstructured) bool {
	if wf.UID == "" || object.GetNamespace() != wf.Namespace {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.Name == wf.Name && owner.UID == wf.UID {
			return true
		}
	}
	return false
}

// PVCs created by CDI may be owned by a run-owned DataVolume instead of by
// the Workflow directly. Bind both links by UID; labels alone prove nothing.
func (c *Controller) runOwnsResource(ctx context.Context, wf *wfv1.Workflow, object *unstructured.Unstructured) (bool, error) {
	if workflowOwnsResource(wf, object) {
		return true, nil
	}
	if object.GetKind() != "PersistentVolumeClaim" || object.GetNamespace() != wf.Namespace {
		return false, nil
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion != "cdi.kubevirt.io/v1beta1" || owner.Kind != "DataVolume" || owner.UID == "" {
			continue
		}
		parent, err := c.vmi.Resource(dataVolumeResource).Namespace(wf.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if parent.GetUID() == owner.UID && workflowOwnsResource(wf, parent) {
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) ensureResourceDelete(ctx context.Context, wf *wfv1.Workflow, pod *corev1.Pod, t *wfv1.Template) error {
	if c.vmi == nil || wf.UID == "" || pod.UID == "" || wf.Namespace != c.config.Namespace || pod.Namespace != wf.Namespace {
		return errors.New("argojob: resource delete guard is unavailable or namespace mismatched")
	}
	if err := c.verifyOwnership(wf, ""); err != nil {
		return err
	}
	owned := false
	for _, owner := range pod.OwnerReferences {
		if owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.Name == wf.Name && owner.UID == wf.UID && owner.Controller != nil && *owner.Controller {
			owned = true
		}
	}
	patched := false
	for _, main := range pod.Spec.Containers {
		if main.Name == "main" && main.Image == argoworkflow.NonrootExecutorImage && guardedResourceCommand(main.Command, main.Args, []string{"argoexec", "version"}) {
			patched = true
		}
	}
	if !owned || !patched {
		return errors.New("argojob: resource delete requires a Workflow-owned Pod and server delete guard; drain legacy VM workflows before upgrading")
	}
	if err := argoworkflow.ValidateVMIResource(t.Resource, c.config.ContainerDiskPrefixes); err != nil {
		return err
	}
	raw, err := yaml.YAMLToJSONStrict([]byte(t.Resource.Manifest))
	if err != nil {
		return err
	}
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &object.Object); err != nil {
		return err
	}
	name := strings.ReplaceAll(object.GetName(), "{{workflow.name}}", wf.Name)
	uid := string(object.GetUID())
	if input := argoworkflow.CleanupUIDInput(uid); input != "" {
		uid = ""
		node := wf.Status.Nodes[pod.Annotations["workflows.argoproj.io/node-id"]]
		if node.Inputs != nil {
			for _, parameter := range node.Inputs.Parameters {
				if parameter.Name == input && parameter.Value != nil {
					uid = parameter.Value.String()
				}
			}
		}
		if !argoworkflow.ValidCleanupUID(uid) {
			return errors.New("argojob: delete UID input is missing or invalid")
		}
	}
	var resource schema.GroupVersionResource
	switch object.GetKind() {
	case "VirtualMachineInstance":
		resource = vmiResource
	case "DataVolume":
		resource = dataVolumeResource
	case "PersistentVolumeClaim":
		resource = pvcResource
	default:
		return errors.New("argojob: unapproved delete resource")
	}
	client := c.vmi.Resource(resource).Namespace(wf.Namespace)
	observed, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if observed.GetUID() == "" || (uid != "" && observed.GetUID() != types.UID(uid)) {
		return fmt.Errorf("argojob: refusing delete of %s/%s: UID precondition differs", object.GetKind(), name)
	}
	owned, err = c.runOwnsResource(ctx, wf, observed)
	if err != nil {
		return err
	}
	if !owned || !strings.HasPrefix(name, wf.Name+"-") {
		return fmt.Errorf("argojob: refusing delete of foreign %s/%s (not created by this run)", object.GetKind(), name)
	}
	pinned := observed.GetUID()
	if observed.GetDeletionTimestamp() == nil {
		if err := client.Delete(ctx, name, cleanupDeleteOptions(resource, pinned)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	current, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("argojob: delete absence readback %s/%s: %w", object.GetKind(), name, err)
	}
	if current.GetUID() != pinned {
		return fmt.Errorf("argojob: foreign replacement remains at %s/%s; refusing delete", object.GetKind(), name)
	}
	return fmt.Errorf("%w: %s/%s uid=%s", errResourceDeletePending, object.GetKind(), name, pinned)
}

func cleanupDeleteOptions(resource schema.GroupVersionResource, uid types.UID) metav1.DeleteOptions {
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if resource == dataVolumeResource {
		policy := metav1.DeletePropagationForeground
		options.PropagationPolicy = &policy
	}
	return options
}

const initialCleanupPollDelay = 100 * time.Millisecond

func nextCleanupPollDelay(delay time.Duration) time.Duration {
	return min(2*delay, 2*time.Second)
}

func waitCleanupPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type cleanupObject struct {
	resource schema.GroupVersionResource
	object   unstructured.Unstructured
}

func cleanupKey(resource schema.GroupVersionResource, name string) string {
	return resource.String() + "/" + name
}

func (o cleanupObject) identity() string {
	return fmt.Sprintf("%s/%s uid=%s", o.object.GetKind(), o.object.GetName(), o.object.GetUID())
}

// API/configuration failures belong to the cleanup observer, not to guest
// tests. Keep a typed cause so leftover/ownership failures stay pipeline red.
type cleanupInfrastructureError struct{ cause error }

func (err *cleanupInfrastructureError) Error() string { return err.cause.Error() }
func (err *cleanupInfrastructureError) Unwrap() error { return err.cause }
func cleanupInfrastructureFailure(err error) error    { return &cleanupInfrastructureError{cause: err} }

// Label-filtered lists discover run resources. An initial and final prefix
// scan also find unlabeled volumeClaimTemplates, source and CDI PVCs. While
// waiting, read those known names directly instead of relisting a shared
// namespace. Only matching run objects count against the inventory bound.
func (c *Controller) cleanupObjects(ctx context.Context, wf *wfv1.Workflow, known map[string]types.UID, scanPrefix bool) ([]cleanupObject, error) {
	var objects []cleanupObject
	for _, resource := range []schema.GroupVersionResource{vmiResource, dataVolumeResource, pvcResource} {
		client := c.vmi.Resource(resource).Namespace(wf.Namespace)
		seen := map[string]bool{}
		add := func(object unstructured.Unstructured) error {
			if object.GetNamespace() != wf.Namespace || !strings.HasPrefix(object.GetName(), wf.Name+"-") || seen[object.GetName()] {
				return nil
			}
			seen[object.GetName()] = true
			if len(seen) > 2048 {
				return fmt.Errorf("run %s exceeds 2048-object cleanup bound for %s", wf.Name, resource.Resource)
			}
			owned := object.GetUID() != "" && known[cleanupKey(resource, object.GetName())] == object.GetUID()
			if !owned {
				var err error
				owned, err = c.runOwnsResource(ctx, wf, &object)
				if err != nil {
					return cleanupInfrastructureFailure(fmt.Errorf("read cleanup ownership: %w", err))
				}
			}
			candidate := cleanupObject{resource: resource, object: object}
			objects = append(objects, candidate)
			if !owned {
				return fmt.Errorf("refusing delete of foreign or unproven %s", candidate.identity())
			}
			return nil
		}
		list := func(selector string) error {
			next := ""
			for {
				if err := ctx.Err(); err != nil {
					return cleanupInfrastructureFailure(err)
				}
				page, err := client.List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 256, Continue: next})
				if apierrors.IsNotFound(err) && resource != pvcResource {
					return nil
				} // Optional CRD absent.
				if err != nil {
					return cleanupInfrastructureFailure(fmt.Errorf("list %s: %w", resource.Resource, err))
				}
				for _, object := range page.Items {
					if err := add(object); err != nil {
						return err
					}
				}
				if page.GetContinue() == "" {
					return nil
				}
				if next == page.GetContinue() {
					return cleanupInfrastructureFailure(errors.New("cleanup list pagination did not advance"))
				}
				next = page.GetContinue()
			}
		}
		if err := list("oberth.ci/workflow=" + wf.Name); err != nil {
			return objects, err
		}
		for key := range known {
			name, matches := strings.CutPrefix(key, resource.String()+"/")
			if !matches || seen[name] {
				continue
			}
			object, err := client.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return objects, cleanupInfrastructureFailure(fmt.Errorf("read cleanup %s/%s: %w", resource.Resource, name, err))
			}
			if err := add(*object); err != nil {
				return objects, err
			}
		}
		if scanPrefix {
			if err := list(""); err != nil {
				return objects, err
			}
		}
	}
	return objects, nil
}

type cleanupLogWriter struct {
	io.Writer
	err error
}

func (writer *cleanupLogWriter) Write(body []byte) (int, error) {
	n, err := writer.Writer.Write(body)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil && writer.err == nil {
		writer.err = err
	}
	return n, err
}

func (c *Controller) cleanupVerdict(ctx context.Context, wf *wfv1.Workflow, runID string, destination io.Writer, completion *Completion) {
	timeout, declarationErr := argoworkflow.CleanupVerdictTimeout(wf)
	if timeout == 0 && declarationErr == nil {
		c.cleanupVMIsForObservedWorkflow(ctx, wf, runID)
		return
	}
	var output *cleanupLogWriter
	if destination != nil {
		output = &cleanupLogWriter{Writer: destination}
		destination = output
	}
	step := StepResult{Burn: "oberth-cleanup", Step: "cleanup-verdict", Status: runprogress.StepRunning}
	for _, existing := range completion.Steps {
		step.Ordinal = max(step.Ordinal, existing.Ordinal+1)
	}
	start := time.Now().UTC()
	step.StartedAt = &start
	emit := func(status runprogress.Status) {
		if destination == nil {
			return
		}
		marker, err := runprogress.MarshalMarker(runprogress.Event{Version: runprogress.Version, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, Status: status, ExitCode: step.ExitCode, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt})
		if err == nil {
			_, _ = destination.Write(marker)
		}
	}
	emit(runprogress.StepRunning)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	err := declarationErr
	if err == nil {
		if ownershipErr := c.verifyOwnership(wf, runID); ownershipErr != nil {
			err = cleanupInfrastructureFailure(ownershipErr)
		}
	}
	if err == nil && (c.vmi == nil || wf.UID == "" || wf.Namespace != c.config.Namespace) {
		err = cleanupInfrastructureFailure(errors.New("cleanup verdict unavailable: missing client or Workflow identity"))
	}
	var remaining []string
	attempted := map[string]bool{}
	known := map[string]types.UID{}
	pollDelay := initialCleanupPollDelay
	for err == nil {
		scanPrefix := len(known) == 0
		objects, readErr := c.cleanupObjects(cleanupCtx, wf, known, scanPrefix)
		if readErr == nil && len(objects) == 0 && !scanPrefix {
			// Catch late unlabeled resources before accepting complete absence.
			objects, readErr = c.cleanupObjects(cleanupCtx, wf, known, true)
		}
		if readErr == nil || len(objects) > 0 {
			remaining = nil
			for _, object := range objects {
				remaining = append(remaining, object.identity())
			}
		}
		if readErr != nil {
			err = readErr
			break
		}
		if len(objects) == 0 {
			break
		}
		for _, object := range objects {
			key := object.identity()
			known[cleanupKey(object.resource, object.object.GetName())] = object.object.GetUID()
			if attempted[key] {
				continue
			}
			uid := object.object.GetUID()
			if uid == "" {
				err = cleanupInfrastructureFailure(fmt.Errorf("cleanup object has no UID: %s", key))
				break
			}
			if destination != nil {
				writeLine(destination, step, "delete "+key)
			}
			deleteErr := c.vmi.Resource(object.resource).Namespace(wf.Namespace).Delete(cleanupCtx, object.object.GetName(), cleanupDeleteOptions(object.resource, uid))
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				err = fmt.Errorf("delete %s: %w", key, deleteErr)
				if !apierrors.IsConflict(deleteErr) {
					err = cleanupInfrastructureFailure(err)
				}
				break
			}
			attempted[key] = true
		}
		if err == nil {
			err = waitCleanupPoll(cleanupCtx, pollDelay)
			pollDelay = nextCleanupPollDelay(pollDelay)
		}
	}
	finish := time.Now().UTC()
	step.FinishedAt = &finish
	step.Status = runprogress.StepPassed
	message := "PASS: run-created VMIs, DataVolumes and PVCs are absent"
	failedMessage := func(err error) string {
		slices.Sort(remaining)
		return fmt.Sprintf("FAIL: cleanup verdict: %v; remaining objects: [%s]", err, strings.Join(remaining, ", "))
	}
	if err != nil {
		message = failedMessage(err)
		step.Status, step.ExitCode = runprogress.StepFailed, 1
	}
	if destination != nil {
		writeLine(destination, step, message)
	}
	emit(step.Status)
	if output != nil && output.err != nil {
		logErr := cleanupInfrastructureFailure(fmt.Errorf("cleanup verdict step log unavailable: %w", output.err))
		if err == nil {
			err = logErr
		} else {
			// Losing the log must not turn an already-proven leftover or
			// ownership failure into a retryable infrastructure failure.
			err = fmt.Errorf("%w; %v", err, logErr)
		}
		message = failedMessage(err)
		step.Status, step.ExitCode = runprogress.StepFailed, 1
	}
	if err != nil {
		pipelineFailed := !completion.Succeeded && !completion.Infrastructure
		var infrastructure *cleanupInfrastructureError
		if errors.As(err, &infrastructure) {
			step.InfrastructureReason = infrastructure.Error()
		}
		completion.Succeeded = false
		completion.Infrastructure = infrastructure != nil && !pipelineFailed
		if !pipelineFailed || completion.FailedStep == "" {
			completion.FailedBurn, completion.FailedStep = step.Burn, step.Step
		}
		completion.Reason = strings.TrimPrefix(strings.TrimSpace(completion.Reason+"; "+message), "; ")
	}
	completion.Steps = append(completion.Steps, step)
}
