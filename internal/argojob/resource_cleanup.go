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

// A mixed failure retains any ownership/residue failure as a pipeline
// failure; a sibling API outage must not make that verdict retryable.
func cleanupIsInfrastructure(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*cleanupInfrastructureError); ok { //nolint:errorlint // errors.As would misclassify a mixed join; inspect each branch below.
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		for _, cause := range causes {
			if !cleanupIsInfrastructure(cause) {
				return false
			}
		}
		return len(causes) != 0
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cleanupIsInfrastructure(wrapped.Unwrap())
	}
	return false
}

type cleanupPendingIdentityError struct{}

func (*cleanupPendingIdentityError) Error() string {
	return "unconfirmed VMI create intent; absence cannot prove an in-flight POST finished"
}

// Label-filtered lists discover run resources. An initial and final prefix
// scan also find unlabeled volumeClaimTemplates, source and CDI PVCs. While
// waiting, read those known names directly instead of relisting a shared
// namespace. Only matching run objects count against the inventory bound.
func (c *Controller) cleanupObjects(ctx context.Context, wf *wfv1.Workflow, known map[string]types.UID, scanPrefix bool) ([]cleanupObject, error) {
	var objects []cleanupObject
	var failures []error
	for _, resource := range []schema.GroupVersionResource{vmiResource, dataVolumeResource, pvcResource} {
		client := c.vmi.Resource(resource).Namespace(wf.Namespace)
		seen := map[string]bool{}
		add := func(object unstructured.Unstructured) error {
			if object.GetNamespace() != wf.Namespace {
				return errors.New("cleanup namespace mismatch")
			}
			if !strings.HasPrefix(object.GetName(), wf.Name+"-") {
				if object.GetLabels()["oberth.ci/workflow"] == wf.Name {
					return errors.New("cleanup run label has invalid name prefix")
				}
				return nil
			}
			if seen[object.GetName()] {
				return nil
			}
			seen[object.GetName()] = true
			if len(seen) > 2048 {
				return fmt.Errorf("run %s exceeds 2048-object cleanup bound for %s", wf.Name, resource.Resource)
			}
			candidate := cleanupObject{resource: resource, object: object}
			pinned := known[cleanupKey(resource, object.GetName())]
			if pinned != "" && pinned != object.GetUID() {
				return fmt.Errorf("replacement UID at %s; original uid=%s", candidate.identity(), pinned)
			}
			if resource == vmiResource && object.GetLabels()["oberth.ci/workflow"] != wf.Name {
				return fmt.Errorf("run label mismatch at %s", candidate.identity())
			}
			owned, err := c.runOwnsResource(ctx, wf, &object)
			if err != nil {
				return cleanupInfrastructureFailure(fmt.Errorf("read cleanup ownership: %w", err))
			}
			// A pinned CDI child can outlive its deleted parent. Other owner
			// checks, including every VMI's Workflow owner, remain mandatory.
			if !owned && resource == pvcResource && pinned == object.GetUID() && pinned != "" {
				owned = true
			}
			if !owned || object.GetUID() == "" {
				return fmt.Errorf("refusing delete of foreign or unproven %s", candidate.identity())
			}
			objects = append(objects, candidate)
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
				}
				if err != nil {
					return cleanupInfrastructureFailure(fmt.Errorf("list %s: %w", resource.Resource, err))
				}
				for _, object := range page.Items {
					if err := add(object); err != nil {
						failures = append(failures, err)
					}
					if len(seen) > 2048 {
						return errors.New("cleanup inventory bound exceeded")
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
			failures = append(failures, err)
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
				failures = append(failures, cleanupInfrastructureFailure(fmt.Errorf("read cleanup %s/%s: %w", resource.Resource, name, err)))
				continue
			}
			if err := add(*object); err != nil {
				failures = append(failures, err)
			}
		}
		if scanPrefix {
			if err := list(""); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return objects, errors.Join(failures...)
}

// Recover original UIDs even after an early delete and after the resource Pod
// has gone. The server ledger is bounded and contains no guest credentials.
func (c *Controller) cleanupIdentities(ctx context.Context, wf *wfv1.Workflow) (map[string]types.UID, error) {
	known := map[string]types.UID{}
	for _, t := range wf.Spec.Templates {
		if t.Resource == nil || t.Resource.Action != "create" {
			continue
		}
		op, err := argoworkflow.VMIOperation(t.Resource)
		if err != nil {
			return known, err
		}
		known[cleanupKey(vmiResource, strings.ReplaceAll(op.Name, "{{workflow.name}}", wf.Name))] = ""
	}
	if c.kube == nil {
		return known, cleanupInfrastructureFailure(errors.New("cleanup Pod/identity observer unavailable"))
	}
	cm, err := c.getVMILedger(ctx)
	if err != nil {
		return known, cleanupInfrastructureFailure(err)
	}
	ledger, err := readVMILedger(cm)
	if err != nil {
		return known, cleanupInfrastructureFailure(err)
	}
	var pending bool
	for _, claim := range ledger.Claims {
		if claim.WorkflowUID != wf.UID {
			continue
		}
		if claim.CleanupWorkflow != wf.Name || claim.CleanupRun != wf.Annotations[runIDAnnotation] || !strings.HasPrefix(claim.Name, wf.Name+"-") {
			return known, errors.New("cleanup creation receipt identity mismatch")
		}
		if claim.UID == "" && !claim.Closed {
			pending = true
		}
		key := cleanupKey(vmiResource, claim.Name)
		if _, exists := known[key]; !exists {
			known[key] = ""
		}
		if previous := known[key]; previous != "" && claim.UID != "" && previous != claim.UID {
			return known, errors.New("multiple original VMI UIDs at one cleanup name")
		}
		if claim.UID != "" {
			known[key] = claim.UID
		}
	}
	if pending {
		return known, &cleanupPendingIdentityError{}
	}
	return known, nil
}

// Launcher Pods own the ephemeral disks. VMI NotFound alone is insufficient:
// independently read the whole Pod collection, including terminating Pods.
func (c *Controller) cleanupLaunchers(ctx context.Context, wf *wfv1.Workflow, known map[string]types.UID) ([]string, error) {
	if c.kube == nil {
		return nil, cleanupInfrastructureFailure(errors.New("cleanup Pod observer unavailable"))
	}
	names, uids := map[string]bool{}, map[types.UID]bool{}
	for key, uid := range known {
		if name, ok := strings.CutPrefix(key, vmiResource.String()+"/"); ok {
			names[name] = true
			if uid != "" {
				uids[uid] = true
			}
		}
	}
	var remaining []string
	next := ""
	for {
		pods, err := c.kube.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{Limit: 256, Continue: next})
		if err != nil {
			return remaining, cleanupInfrastructureFailure(fmt.Errorf("read launcher absence: %w", err))
		}
		for _, pod := range pods.Items {
			match := uids[types.UID(pod.Labels["kubevirt.io/created-by"])] || names[pod.Labels["kubevirt.io/domain"]] || strings.HasPrefix(pod.Name, "virt-launcher-"+wf.Name+"-")
			for _, owner := range pod.OwnerReferences {
				if owner.APIVersion == "kubevirt.io/v1" && owner.Kind == "VirtualMachineInstance" && (uids[owner.UID] || names[owner.Name]) {
					match = true
				}
			}
			if match {
				remaining = append(remaining, fmt.Sprintf("Pod/%s uid=%s", pod.Name, pod.UID))
				if len(remaining) > 2048 {
					return remaining, errors.New("cleanup launcher inventory bound exceeded")
				}
			}
		}
		if pods.Continue == "" {
			return remaining, nil
		}
		if pods.Continue == next {
			return remaining, cleanupInfrastructureFailure(errors.New("cleanup Pod pagination did not advance"))
		}
		next = pods.Continue
	}
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
	if err == nil {
		known, err = c.cleanupIdentities(cleanupCtx, wf)
	}
	var identityFailure error
	var pending *cleanupPendingIdentityError
	if errors.As(err, &pending) {
		identityFailure, err = err, nil
	}
	if destination != nil {
		writeLine(destination, step, fmt.Sprintf("cleanup receipt run=%s Workflow/%s namespace=%s uid=%s", runID, wf.Name, wf.Namespace, wf.UID))
		keys := make([]string, 0, len(known))
		for key := range known {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			writeLine(destination, step, fmt.Sprintf("original %s uid=%s", key, known[key]))
		}
	}
	firstScan := true
	pollDelay := initialCleanupPollDelay
	for err == nil {
		scanPrefix := firstScan
		firstScan = false
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
		// Inventory failures stay failed, but cannot suppress safe sibling
		// deletion attempts from the same independent inventory.
		err = errors.Join(identityFailure, readErr)
		for _, object := range objects {
			key := object.identity()
			known[cleanupKey(object.resource, object.object.GetName())] = object.object.GetUID()
			if attempted[key] {
				continue
			}
			uid := object.object.GetUID()
			if uid == "" {
				err = errors.Join(err, cleanupInfrastructureFailure(fmt.Errorf("cleanup object has no UID: %s", key)))
				continue
			}
			if destination != nil {
				writeLine(destination, step, "delete "+key)
			}
			deleteErr := c.vmi.Resource(object.resource).Namespace(wf.Namespace).Delete(cleanupCtx, object.object.GetName(), cleanupDeleteOptions(object.resource, uid))
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				failure := fmt.Errorf("delete %s: %w", key, deleteErr)
				if !apierrors.IsConflict(deleteErr) {
					failure = cleanupInfrastructureFailure(failure)
				}
				err = errors.Join(err, failure)
				continue
			}
			attempted[key] = true
		}
		launchers, launcherErr := c.cleanupLaunchers(cleanupCtx, wf, known)
		remaining = append(remaining, launchers...)
		err = errors.Join(err, launcherErr)
		if err == nil && len(objects) == 0 && len(launchers) == 0 {
			break
		}
		if err == nil {
			err = waitCleanupPoll(cleanupCtx, pollDelay)
			pollDelay = nextCleanupPollDelay(pollDelay)
		}
	}
	finish := time.Now().UTC()
	step.FinishedAt = &finish
	step.Status = runprogress.StepPassed
	message := "PASS: independent API read confirms original VMI names, virt-launcher Pods, DataVolumes and PVCs absent"
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
			err = fmt.Errorf("%w; %w", err, logErr)
		}
		message = failedMessage(err)
		step.Status, step.ExitCode = runprogress.StepFailed, 1
	}
	if err != nil {
		pipelineFailed := !completion.Succeeded && !completion.Infrastructure
		infrastructure := cleanupIsInfrastructure(err)
		if infrastructure {
			step.InfrastructureReason = err.Error()
		}
		completion.Succeeded = false
		completion.Infrastructure = infrastructure && !pipelineFailed
		if !pipelineFailed || completion.FailedStep == "" {
			completion.FailedBurn, completion.FailedStep = step.Burn, step.Step
		}
		completion.Reason = strings.TrimPrefix(strings.TrimSpace(completion.Reason+"; "+message), "; ")
	}
	completion.Steps = append(completion.Steps, step)
}
