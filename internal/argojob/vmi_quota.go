package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const (
	vmiQuotaAnnotation      = "oberth.ci/vmi-quota"
	vmiRepositoryLabel      = "oberth.ci/repository-id"
	vmiOperationAnnotation  = "oberth.ci/vmi-operation"
	vmiCreateHashAnnotation = "oberth.ci/vmi-create-sha256"
	vmiQuotaConfigMap       = "oberth-vmi-reservations"
	maxVMIClaims            = 2048
)

// Public, server-generated and included in the immutable submission identity.
// Existing grants/defaults are unchanged; a running workflow retains its
// admitted limit, just as it does after grant revocation today.
type vmiQuotaBinding struct {
	RepositoryID int64                  `json:"repository_id"`
	Limit        argoworkflow.VMIDemand `json:"limit"`
	Peak         argoworkflow.VMIDemand `json:"peak"`
}

type vmiClaim struct {
	RepositoryID int64                  `json:"repository_id"`
	PodName      string                 `json:"pod_name"`
	PodUID       types.UID              `json:"pod_uid"`
	WorkflowUID  types.UID              `json:"workflow_uid"`
	Name         string                 `json:"name"`
	Hash         string                 `json:"hash"`
	Demand       argoworkflow.VMIDemand `json:"demand"`
	UID          types.UID              `json:"uid,omitempty"`
	Closed       bool                   `json:"closed,omitempty"`
}

type vmiLedger struct {
	Version int                 `json:"version"`
	Claims  map[string]vmiClaim `json:"claims"`
}

// Argo's resource create becomes a get in the submitted Pod. Only Oberth may
// perform the actual create, after acquiring a persistent quota claim. The
// launcher account has no create verb, so the Pod cannot bypass this decision.
func projectVMICreateGuard(wf *wfv1.Workflow, config Config, request Request) error {
	if !argoworkflow.DeclaresVMIResources(wf) {
		return nil
	}
	if _, authored := wf.Annotations[vmiQuotaAnnotation]; authored {
		return errors.New("argojob: VMI quota binding is server-owned")
	}
	if request.RepositoryID <= 0 {
		return errors.New("argojob: VMI creation requires the registered repository ID")
	}
	grant, ceiling := request.KubeVirtBudget, config.KubeVirtCeiling
	if grant.IsZero() {
		grant = argoworkflow.DefaultVMIBudget()
	}
	if ceiling.IsZero() {
		ceiling = argoworkflow.VMIBudgetLimit()
	}
	peak, err := argoworkflow.DeclaredVMIDemand(wf)
	if err != nil {
		return err
	}
	binding := vmiQuotaBinding{request.RepositoryID, grant.Within(ceiling).DemandLimit(), peak}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	wf.Annotations[vmiQuotaAnnotation] = string(encoded)
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		if t.Resource == nil || t.Resource.Action != "create" {
			continue
		}
		patch := map[string]any{}
		if t.PodSpecPatch != "" {
			if err := json.Unmarshal([]byte(t.PodSpecPatch), &patch); err != nil {
				return err
			}
		}
		containers, _ := patch["containers"].([]any)
		found := false
		for _, item := range containers {
			if c, ok := item.(map[string]any); ok && c["name"] == "main" {
				c["command"] = []string{"argoexec", "resource", "get"}
				c["args"] = []string{}
				found = true
			}
		}
		if !found {
			containers = append(containers, map[string]any{"name": "main", "command": []string{"argoexec", "resource", "get"}, "args": []string{}})
		}
		patch["containers"] = containers
		body, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		t.PodSpecPatch = string(body)
	}
	return nil
}

// Argo wraps main in emissary after applying podSpecPatch. Accept only its
// logging flags followed by the exact get command, or the unwrapped command.
func guardedVMIGetCommand(command, args []string) bool {
	if len(args) != 0 {
		return false
	}
	want := []string{"argoexec", "resource", "get"}
	if slices.Equal(command, want) {
		return true
	}
	if len(command) < 6 || command[0] != "/var/run/argo/argoexec" || command[1] != "emissary" {
		return false
	}
	i := 2
	for i < len(command) && command[i] != "--" {
		if i+1 >= len(command) || (command[i] != "--loglevel" && command[i] != "--log-format" && command[i] != "--gloglevel") {
			return false
		}
		i += 2
	}
	return i < len(command) && slices.Equal(command[i+1:], want)
}

func guardedResourceTemplate(wf *wfv1.Workflow, pod *corev1.Pod) *wfv1.Template {
	node, ok := wf.Status.Nodes[pod.Annotations["workflows.argoproj.io/node-id"]]
	if !ok {
		return nil
	}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		if t.Name == node.TemplateName && t.Resource != nil {
			return t
		}
	}
	return nil
}

// Called at the resource Pod's token barrier, before its executor can run.
func (c *Controller) ensureVMIForResourcePod(ctx context.Context, wf *wfv1.Workflow, pod *corev1.Pod) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t := guardedResourceTemplate(wf, pod)
	if t == nil || t.Resource.Action != "create" {
		return nil
	}
	if c.kube == nil || c.vmi == nil || c.vmiCreate == nil || pod.UID == "" || wf.UID == "" {
		return errors.New("argojob: VMI create guard is unavailable")
	}
	if wf.Namespace != c.config.Namespace || pod.Namespace != wf.Namespace {
		return errors.New("argojob: VMI create namespace mismatch")
	}
	owned := false
	for _, owner := range pod.OwnerReferences {
		if owner.UID == wf.UID && owner.Kind == "Workflow" && owner.Controller != nil && *owner.Controller {
			owned = true
		}
	}
	if !owned {
		return errors.New("argojob: VMI create Pod is not owned by the Workflow")
	}
	if err := c.verifyOwnership(wf, ""); err != nil {
		return err
	}
	patched := false
	for _, main := range pod.Spec.Containers {
		if main.Name == "main" && main.Image == argoworkflow.NonrootExecutorImage && guardedVMIGetCommand(main.Command, main.Args) {
			patched = true
		}
	}
	if !patched {
		return errors.New("argojob: VMI create Pod lacks the server-owned get guard; drain legacy VM workflows before upgrading")
	}
	var binding vmiQuotaBinding
	if err := json.Unmarshal([]byte(wf.Annotations[vmiQuotaAnnotation]), &binding); err != nil || binding.RepositoryID <= 0 || !validVMIDemand(binding.Limit) || !validVMIDemand(binding.Peak) || !binding.Peak.Fits(binding.Limit) {
		return errors.New("argojob: invalid server VMI quota binding")
	}
	if err := argoworkflow.ValidateVMIResource(t.Resource, c.config.ContainerDiskPrefixes); err != nil {
		return err
	}
	op, err := argoworkflow.VMIOperation(t.Resource)
	if err != nil {
		return err
	}
	if !op.Demand.Fits(binding.Peak) {
		return errors.New("argojob: VMI create exceeds the workflow reservation")
	}
	raw, err := yaml.YAMLToJSONStrict([]byte(strings.ReplaceAll(t.Resource.Manifest, "{{workflow.name}}", wf.Name)))
	if err != nil {
		return err
	}
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &object.Object); err != nil {
		return err
	}
	object.SetNamespace(wf.Namespace)
	object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}})
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[vmiRepositoryLabel] = fmt.Sprint(binding.RepositoryID)
	object.SetLabels(labels)
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	object.SetAnnotations(map[string]string{vmiCreateHashAnnotation: hash, vmiOperationAnnotation: string(pod.UID)})
	keyHash := sha256.Sum256([]byte(pod.UID))
	key := hex.EncodeToString(keyHash[:])
	desired := vmiClaim{PodName: pod.Name, PodUID: pod.UID, RepositoryID: binding.RepositoryID, WorkflowUID: wf.UID, Name: object.GetName(), Hash: hash, Demand: op.Demand}
	claim, fresh, err := c.reserveVMIClaim(ctx, key, desired, binding)
	if err != nil {
		return fmt.Errorf("argojob: VMI %s repository %d template %s: %w", desired.Name, desired.RepositoryID, t.Name, err)
	}
	client := c.vmi.Resource(vmiResource).Namespace(wf.Namespace)
	if !fresh {
		if claim.Closed {
			return errors.New("argojob: prior VMI create is closed")
		}
		observed, err := client.Get(ctx, claim.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("argojob: unresolved VMI create intent retained; refusing another create: %w", err)
		}
		if (claim.UID != "" && claim.UID != observed.GetUID()) || !matchesCreatedVMI(observed, wf, desired) {
			return errors.New("argojob: existing VMI differs from the reserved create")
		}
		return c.finishVMIClaim(ctx, key, observed.GetUID(), false)
	}
	created, err := c.vmiCreate(ctx, wf.Namespace, object)
	if err != nil {
		// Only explicit rejections prove no object was created. Transport errors,
		// cancellations, timeouts and 5xx keep the durable pending slot, including
		// across restart. They never earn a retry or an absence-based refund.
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsNotFound(err) || apierrors.IsAlreadyExists(err) {
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			closeErr := c.finishVMIClaim(closeCtx, key, "", true)
			closeCancel()
			if closeErr != nil {
				return errors.Join(err, closeErr)
			}
		}
		return fmt.Errorf("argojob: guarded VMI create: %w", err)
	}
	if !matchesCreatedVMI(created, wf, desired) {
		return errors.New("argojob: VMI create returned an unexpected identity; pending slot retained")
	}
	return c.finishVMIClaim(ctx, key, created.GetUID(), false)
}

func matchesCreatedVMI(object *unstructured.Unstructured, wf *wfv1.Workflow, claim vmiClaim) bool {
	if object == nil || object.GetUID() == "" || object.GetName() != claim.Name || object.GetNamespace() != wf.Namespace || object.GetAnnotations()[vmiCreateHashAnnotation] != claim.Hash || object.GetAnnotations()[vmiOperationAnnotation] != string(claim.PodUID) || object.GetLabels()[vmiRepositoryLabel] != fmt.Sprint(claim.RepositoryID) {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == wf.UID && owner.Kind == "Workflow" && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func validVMIDemand(d argoworkflow.VMIDemand) bool {
	return d.VMs > 0 && d.Cores > 0 && d.MemoryBytes > 0 && d.Fits(argoworkflow.VMIBudgetLimit().DemandLimit())
}

func addVMIDemand(a, b argoworkflow.VMIDemand) argoworkflow.VMIDemand {
	add := func(a, b int64) int64 {
		if b > math.MaxInt64-a {
			return math.MaxInt64
		}
		return a + b
	}
	a.VMs = int(min(int64(math.MaxInt), add(int64(a.VMs), int64(b.VMs))))
	a.Cores = int(min(int64(math.MaxInt), add(int64(a.Cores), int64(b.Cores))))
	a.MemoryBytes = add(a.MemoryBytes, b.MemoryBytes)
	return a
}

func readVMILedger(cm *corev1.ConfigMap) (vmiLedger, error) {
	var ledger vmiLedger
	if cm.Labels["oberth.ci/role"] != "vmi-reservations" || len(cm.Data["reservations.json"]) > 1<<20 {
		return ledger, errors.New("argojob: invalid VMI reservation ledger")
	}
	if err := json.Unmarshal([]byte(cm.Data["reservations.json"]), &ledger); err != nil || ledger.Version != 1 || ledger.Claims == nil || len(ledger.Claims) > maxVMIClaims {
		return ledger, errors.New("argojob: unreadable VMI reservation ledger")
	}
	for _, r := range ledger.Claims {
		if r.RepositoryID <= 0 || r.WorkflowUID == "" || r.PodUID == "" || r.PodName == "" || r.Name == "" || len(r.Hash) != 64 || !validVMIDemand(r.Demand) {
			return ledger, errors.New("argojob: invalid VMI reservation claim")
		}
	}
	return ledger, nil
}

func (c *Controller) getVMILedger(ctx context.Context) (*corev1.ConfigMap, error) {
	client := c.kube.CoreV1().ConfigMaps(c.config.Namespace)
	cm, err := client.Get(ctx, vmiQuotaConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		body, _ := json.Marshal(vmiLedger{Version: 1, Claims: map[string]vmiClaim{}})
		cm, err = client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: vmiQuotaConfigMap, Namespace: c.config.Namespace, Labels: map[string]string{"oberth.ci/role": "vmi-reservations"}}, Data: map[string]string{"reservations.json": string(body)}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return client.Get(ctx, vmiQuotaConfigMap, metav1.GetOptions{})
		}
	}
	return cm, err
}

func (c *Controller) updateVMILedger(ctx context.Context, cm *corev1.ConfigMap, ledger vmiLedger) error {
	body, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return errors.New("argojob: VMI reservation ledger is full")
	}
	cm.Data["reservations.json"] = string(body)
	_, err = c.kube.CoreV1().ConfigMaps(c.config.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// ResourceVersion CAS serializes claims across concurrent observers/processes.
// Live VMIs and pending POSTs count before a new create can be issued. Confirmed
// deleted UIDs become closed tombstones; a pending POST is never cleared merely
// because a GET currently finds no VMI (the POST may still be in flight).
func (c *Controller) reserveVMIClaim(ctx context.Context, key string, want vmiClaim, binding vmiQuotaBinding) (vmiClaim, bool, error) {
	c.vmiQuotaMu.Lock()
	defer c.vmiQuotaMu.Unlock()
	for range 8 {
		cm, err := c.getVMILedger(ctx)
		if err != nil {
			return vmiClaim{}, false, err
		}
		ledger, err := readVMILedger(cm)
		if err != nil {
			return vmiClaim{}, false, err
		}
		if old, ok := ledger.Claims[key]; ok {
			if old.PodName != want.PodName || old.RepositoryID != want.RepositoryID || old.WorkflowUID != want.WorkflowUID || old.Name != want.Name || old.Hash != want.Hash || old.Demand != want.Demand {
				return vmiClaim{}, false, errors.New("argojob: VMI create intent changed")
			}
			return old, false, nil
		}
		// A stale observer must not resurrect an operation after its closed
		// receipt was pruned. Pod UID existence is checked inside this CAS
		// attempt, before creating any new intent.
		currentPod, err := c.kube.CoreV1().Pods(c.config.Namespace).Get(ctx, want.PodName, metav1.GetOptions{})
		if err != nil {
			return vmiClaim{}, false, err
		}
		if currentPod.UID != want.PodUID || currentPod.DeletionTimestamp != nil {
			return vmiClaim{}, false, errors.New("argojob: VMI create operation Pod is gone or replaced")
		}
		// A completed operation's tombstone is needed only while its Pod can
		// return to the identity barrier. Pending/ambiguous POSTs are never pruned.
		pods, podErr := c.kube.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow", Limit: 2048})
		if podErr != nil {
			return vmiClaim{}, false, podErr
		}
		if pods.Continue == "" {
			present := map[types.UID]bool{}
			for _, p := range pods.Items {
				present[p.UID] = true
			}
			for k, r := range ledger.Claims {
				if r.Closed && !present[r.PodUID] {
					delete(ledger.Claims, k)
				}
			}
		}

		objects, err := c.vmi.Resource(vmiResource).Namespace(c.config.Namespace).List(ctx, metav1.ListOptions{Limit: 2048})
		if err != nil {
			return vmiClaim{}, false, err
		}
		if objects.GetContinue() != "" || len(objects.Items) > 2048 {
			return vmiClaim{}, false, errors.New("argojob: too many live VMIs for bounded quota inspection")
		}
		actual := map[string]*unstructured.Unstructured{}
		var repoUsed, workflowUsed argoworkflow.VMIDemand
		for i := range objects.Items {
			object := &objects.Items[i]
			actual[object.GetName()] = object
			// Old/unattributable VMIs are conservatively charged against every repo.
			repo := object.GetLabels()[vmiRepositoryLabel]
			if repo != "" && repo != fmt.Sprint(want.RepositoryID) {
				continue
			}
			demand, err := liveVMIDemand(object)
			if err != nil {
				return vmiClaim{}, false, err
			}
			repoUsed = addVMIDemand(repoUsed, demand)
			for _, owner := range object.GetOwnerReferences() {
				if owner.UID == want.WorkflowUID && owner.Kind == "Workflow" {
					workflowUsed = addVMIDemand(workflowUsed, demand)
					break
				}
			}
		}
		for k, claim := range ledger.Claims {
			if claim.Closed {
				continue
			}
			object := actual[claim.Name]
			if claim.UID != "" {
				if object == nil {
					claim.Closed = true
					ledger.Claims[k] = claim
					continue
				}
				if object.GetUID() != claim.UID {
					return vmiClaim{}, false, errors.New("argojob: reserved VMI UID changed")
				}
			}
			if object != nil {
				if object.GetLabels()[vmiRepositoryLabel] != fmt.Sprint(claim.RepositoryID) || object.GetAnnotations()[vmiCreateHashAnnotation] != claim.Hash || object.GetAnnotations()[vmiOperationAnnotation] != string(claim.PodUID) {
					return vmiClaim{}, false, errors.New("argojob: live VMI differs from pending reservation")
				}
				continue // Actual object was counted above; do not count its claim twice.
			}
			if claim.RepositoryID == want.RepositoryID {
				repoUsed = addVMIDemand(repoUsed, claim.Demand)
			}
			if claim.WorkflowUID == want.WorkflowUID {
				workflowUsed = addVMIDemand(workflowUsed, claim.Demand)
			}
		}
		if pods.Continue == "" {
			present := map[types.UID]bool{}
			for _, p := range pods.Items {
				present[p.UID] = true
			}
			for k, r := range ledger.Claims {
				if r.Closed && !present[r.PodUID] {
					delete(ledger.Claims, k)
				}
			}
		}
		if len(ledger.Claims) >= maxVMIClaims {
			return vmiClaim{}, false, errors.New("argojob: VMI reservation ledger is full")
		}
		if !addVMIDemand(repoUsed, want.Demand).Fits(binding.Limit) || !addVMIDemand(workflowUsed, want.Demand).Fits(binding.Peak) {
			return vmiClaim{}, false, fmt.Errorf("argojob: VMI create refused over grant or workflow peak: repository used=%+v requested=%+v limit=%+v workflow used=%+v peak=%+v", repoUsed, want.Demand, binding.Limit, workflowUsed, binding.Peak)
		}
		ledger.Claims[key] = want
		err = c.updateVMILedger(ctx, cm, ledger)
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return vmiClaim{}, false, err
		}
		return want, true, nil
	}
	return vmiClaim{}, false, errors.New("argojob: VMI reservation conflicts exhausted")
}

func (c *Controller) finishVMIClaim(ctx context.Context, key string, uid types.UID, closed bool) error {
	c.vmiQuotaMu.Lock()
	defer c.vmiQuotaMu.Unlock()
	for range 8 {
		cm, err := c.getVMILedger(ctx)
		if err != nil {
			return err
		}
		ledger, err := readVMILedger(cm)
		if err != nil {
			return err
		}
		claim, ok := ledger.Claims[key]
		if !ok {
			return errors.New("argojob: VMI create lost its persistent reservation")
		}
		if claim.UID != "" && claim.UID != uid {
			return errors.New("argojob: VMI reservation UID changed")
		}
		claim.UID, claim.Closed = uid, closed
		ledger.Claims[key] = claim
		err = c.updateVMILedger(ctx, cm, ledger)
		if apierrors.IsConflict(err) {
			continue
		}
		return err
	}
	return errors.New("argojob: VMI reservation completion conflicts exhausted")
}

// WithVMICreateClient uses exactly one POST. Automatic POST retries would leave
// more than one potentially in-flight create behind one durable claim after a
// transport failure, invalidating absence/recovery proofs when a name is reused.
func (c *Controller) WithVMICreateClient(client rest.Interface) *Controller {
	c.vmiCreate = func(ctx context.Context, namespace string, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		result := &unstructured.Unstructured{}
		err := client.Post().AbsPath("/apis/kubevirt.io/v1/namespaces", namespace, "virtualmachineinstances").MaxRetries(0).Body(object).Do(ctx).Into(result)
		return result, err
	}
	return c
}
