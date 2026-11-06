package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/oberthci/oberth/internal/model"
)

const targetBindingAnnotation = "oberth.ci/execution-target"

type targetUncertainError struct{}

func (*targetUncertainError) Error() string {
	return "argojob: retained execution target custody is unresolved"
}
func (*targetUncertainError) AmbiguousCreate() bool { return true }

var ErrTargetUncertain error = &targetUncertainError{}

// TargetCustody is canonical durable state. Prepare precedes every possible
// POST; identical intent is not permission to repeat an ambiguous POST.
type TargetCustody interface {
	Binding() model.ExecutionTargetBinding
	Prepare(context.Context, model.ExecutionTargetResource) (bool, error)
	Observe(context.Context, string, string, string, string) error
	Resources(context.Context) ([]model.ExecutionTargetResource, error)
	RetainOwned(context.Context, model.ExecutionTargetResource, string) error
	Cancelled(context.Context) (bool, error)
}

type TargetClients struct {
	config    Config
	workflows WorkflowClient
	kube      kubernetes.Interface
	journal   TargetCustody
	digest    string
	now       func() time.Time
}

// NewTargetClients wraps exactly one already-authenticated bundle. It never
// discovers another cluster or holds any local fallback client.
func NewTargetClients(workflows WorkflowClient, kube kubernetes.Interface, journal TargetCustody, config Config) (*TargetClients, error) {
	if workflows == nil || kube == nil || journal == nil {
		return nil, errors.New("argojob: complete target clients and canonical custody required")
	}
	body, err := json.Marshal(journal.Binding())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return &TargetClients{workflows: workflows, kube: kube, journal: journal, config: config, digest: hex.EncodeToString(sum[:]), now: time.Now}, nil
}

func (c *TargetClients) WorkflowClient() WorkflowClient { return &targetWorkflows{clients: c} }
func (c *TargetClients) Kubernetes() kubernetes.Interface {
	return &targetKubernetes{Interface: c.kube, clients: c}
}

// Observation and disposal retain the original cluster anchor after admission
// expires. They cannot authorize new workflow code or move a run to a replacement
// cluster; every object operation additionally pins its original UID.
func (c *TargetClients) verifyCluster(ctx context.Context) error {
	p := c.config.ExecutionPlacement
	if p == nil {
		return nil
	}
	anchor, err := c.kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("%w: original cluster anchor unavailable: %w", ErrTargetUncertain, err)
	}
	if string(anchor.UID) != p.document.ClusterAnchorUID || anchor.DeletionTimestamp != nil {
		return fmt.Errorf("%w: original cluster anchor replaced", ErrTargetUncertain)
	}
	return nil
}

func (c *TargetClients) resource(ctx context.Context, kind, namespace, name string) (model.ExecutionTargetResource, bool, error) {
	rows, err := c.journal.Resources(ctx)
	if err != nil {
		return model.ExecutionTargetResource{}, false, err
	}
	for _, r := range rows {
		if r.Kind == kind && r.Namespace == namespace && r.Name == name {
			return r, true, nil
		}
	}
	return model.ExecutionTargetResource{}, false, nil
}
func (c *TargetClients) prepare(ctx context.Context, kind string, object metav1.Object) (bool, error) {
	if err := c.config.ExecutionPlacement.ValidateAdmission(); err != nil {
		return false, err
	}
	if err := c.verifyCluster(ctx); err != nil {
		return false, err
	}
	b := c.journal.Binding()
	allowedName := object.GetName() == sourceClaimName(b.Workflow) || (kind == "Pod" && object.GetName() == sourceClaimName(b.Workflow)+collectPodSuffix)
	if pod, ok := object.(*corev1.Pod); ok && pod.Labels["oberth.ci/role"] == "executor-identity" {
		rows, err := c.journal.Resources(ctx)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.Kind == "Workflow" && row.UID != "" && ownedByTarget(pod.OwnerReferences, row.UID) && len(pod.OwnerReferences) == 1 && pod.OwnerReferences[0].Name == b.Workflow && pod.OwnerReferences[0].Controller != nil && *pod.OwnerReferences[0].Controller && pod.Spec.ServiceAccountName == c.config.ExecutorServiceAccount && pod.Name == (&Controller{config: c.config}).identityPodName(types.UID(row.UID), pod.Spec.ServiceAccountName) {
				allowedName = true
			}
		}
	}
	if object.GetNamespace() != b.Namespace || (kind == "Workflow" && object.GetName() != b.Workflow) || (kind != "Workflow" && !allowedName) {
		return false, errors.New("argojob: target create name or namespace differs")
	}
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[targetBindingAnnotation] = c.digest
	object.SetAnnotations(annotations)
	body, err := json.Marshal(object)
	if err != nil {
		return false, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return false, errors.New("argojob: target create requires a bounded transport deadline")
	}
	return c.journal.Prepare(ctx, model.ExecutionTargetResource{RunID: b.RunID, Kind: kind, Namespace: b.Namespace, Name: object.GetName(), Object: body, NotBefore: deadline.Add(time.Minute)})
}
func (c *TargetClients) verify(ctx context.Context, kind string, object metav1.Object) error {
	if err := c.verifyCluster(ctx); err != nil {
		return err
	}
	r, found, err := c.resource(ctx, kind, object.GetNamespace(), object.GetName())
	if err != nil {
		return err
	}
	if !found || object.GetUID() == "" || object.GetAnnotations()[targetBindingAnnotation] != c.digest || (r.UID != "" && r.UID != string(object.GetUID())) {
		return fmt.Errorf("%w: target object identity differs", ErrTargetUncertain)
	}
	switch actual := object.(type) {
	case *wfv1.Workflow:
		var intended wfv1.Workflow
		if json.Unmarshal(r.Object, &intended) != nil || !apiequality.Semantic.DeepEqual(actual.Spec, intended.Spec) {
			return fmt.Errorf("%w: retained Workflow spec differs", ErrTargetUncertain)
		}
		if err := sameSubmission(actual, &intended); err != nil {
			return err
		}
		for key, value := range intended.Annotations {
			if actual.Annotations[key] != value {
				return fmt.Errorf("%w: retained Workflow annotation differs", ErrTargetUncertain)
			}
		}
		for key, value := range intended.Labels {
			if actual.Labels[key] != value {
				return fmt.Errorf("%w: retained Workflow label differs", ErrTargetUncertain)
			}
		}
	case *corev1.PersistentVolumeClaim:
		var intended corev1.PersistentVolumeClaim
		if json.Unmarshal(r.Object, &intended) != nil || !apiequality.Semantic.DeepEqual(actual.Spec.AccessModes, intended.Spec.AccessModes) || !apiequality.Semantic.DeepEqual(actual.Spec.Resources, intended.Spec.Resources) || !apiequality.Semantic.DeepEqual(actual.Spec.StorageClassName, intended.Spec.StorageClassName) {
			return fmt.Errorf("%w: source storage differs", ErrTargetUncertain)
		}
	}
	return c.journal.Observe(ctx, kind, object.GetNamespace(), object.GetName(), string(object.GetUID()))
}
func (c *TargetClients) deletion(ctx context.Context, kind, namespace, name string, object metav1.Object, options metav1.DeleteOptions) (metav1.DeleteOptions, error) {
	if err := c.verify(ctx, kind, object); err != nil {
		return options, err
	}
	uid := object.GetUID()
	if options.Preconditions != nil && options.Preconditions.UID != nil && *options.Preconditions.UID != uid {
		return options, ErrTargetUncertain
	}
	options.Preconditions = &metav1.Preconditions{UID: &uid}
	return options, nil
}

type targetWorkflows struct{ clients *TargetClients }

func (w *targetWorkflows) Create(ctx context.Context, object *wfv1.Workflow, options metav1.CreateOptions) (*wfv1.Workflow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	intended := object.DeepCopy()
	first, err := w.clients.prepare(ctx, "Workflow", intended)
	if err != nil {
		return nil, err
	}
	if !first {
		existing, err := w.Get(ctx, object.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("%w: observe earlier Workflow POST: %w", ErrTargetUncertain, err)
		}
		return existing, nil
	}
	created, err := w.clients.workflows.Create(ctx, intended, options)
	if err != nil {
		return nil, fmt.Errorf("%w: Workflow POST: %w", ErrTargetUncertain, err)
	}
	if err := w.clients.verify(ctx, "Workflow", created); err != nil {
		return nil, err
	}
	return created, nil
}
func (w *targetWorkflows) Get(ctx context.Context, name string, options metav1.GetOptions) (*wfv1.Workflow, error) {
	if err := w.clients.verifyCluster(ctx); err != nil {
		return nil, err
	}
	object, err := w.clients.workflows.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}
	if err := w.clients.verify(ctx, "Workflow", object); err != nil {
		return nil, err
	}
	return object, nil
}
func (w *targetWorkflows) Delete(ctx context.Context, name string, options metav1.DeleteOptions) error {
	object, err := w.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	options, err = w.clients.deletion(ctx, "Workflow", object.Namespace, name, object, options)
	if err != nil {
		return err
	}
	return w.clients.workflows.Delete(ctx, name, options)
}

type targetKubernetes struct {
	kubernetes.Interface
	clients *TargetClients
}

func (k *targetKubernetes) CoreV1() typedcore.CoreV1Interface {
	return &targetCore{CoreV1Interface: k.Interface.CoreV1(), clients: k.clients}
}

type targetCore struct {
	typedcore.CoreV1Interface
	clients *TargetClients
}

func (k *targetCore) Pods(namespace string) typedcore.PodInterface {
	return &targetPods{PodInterface: k.CoreV1Interface.Pods(namespace), clients: k.clients, namespace: namespace}
}
func (k *targetCore) PersistentVolumeClaims(namespace string) typedcore.PersistentVolumeClaimInterface {
	return &targetClaims{PersistentVolumeClaimInterface: k.CoreV1Interface.PersistentVolumeClaims(namespace), clients: k.clients, namespace: namespace}
}

type targetPods struct {
	typedcore.PodInterface
	clients   *TargetClients
	namespace string
}

func (p *targetPods) Create(ctx context.Context, object *corev1.Pod, options metav1.CreateOptions) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	intended := object.DeepCopy()
	first, err := p.clients.prepare(ctx, "Pod", intended)
	if err != nil {
		return nil, err
	}
	if !first {
		existing, err := p.Get(ctx, object.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("%w: observe earlier seed POST: %w", ErrTargetUncertain, err)
		}
		return existing, nil
	}
	created, err := p.PodInterface.Create(ctx, intended, options)
	if err != nil {
		return nil, fmt.Errorf("%w: seed POST: %w", ErrTargetUncertain, err)
	}
	if err := p.clients.verify(ctx, "Pod", created); err != nil {
		return nil, err
	}
	created = created.DeepCopy()
	delete(created.Annotations, targetBindingAnnotation)
	return created, nil
}
func (p *targetPods) Get(ctx context.Context, name string, options metav1.GetOptions) (*corev1.Pod, error) {
	if err := p.clients.verifyCluster(ctx); err != nil {
		return nil, err
	}
	object, err := p.PodInterface.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}
	_, found, err := p.clients.resource(ctx, "Pod", p.namespace, name)
	if err != nil {
		return nil, err
	}
	if found {
		if err := p.clients.verify(ctx, "Pod", object); err != nil {
			return nil, err
		}
		object = object.DeepCopy()
		delete(object.Annotations, targetBindingAnnotation)
	}
	return object, nil
}
func (p *targetPods) Delete(ctx context.Context, name string, options metav1.DeleteOptions) error {
	object, err := p.PodInterface.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	options, err = p.clients.deletion(ctx, "Pod", p.namespace, name, object, options)
	if err != nil {
		return err
	}
	return p.PodInterface.Delete(ctx, name, options)
}

type targetClaims struct {
	typedcore.PersistentVolumeClaimInterface
	clients   *TargetClients
	namespace string
}

func (p *targetClaims) Create(ctx context.Context, object *corev1.PersistentVolumeClaim, options metav1.CreateOptions) (*corev1.PersistentVolumeClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	intended := object.DeepCopy()
	first, err := p.clients.prepare(ctx, "PersistentVolumeClaim", intended)
	if err != nil {
		return nil, err
	}
	if !first {
		existing, err := p.Get(ctx, object.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("%w: observe earlier source POST: %w", ErrTargetUncertain, err)
		}
		return existing, nil
	}
	created, err := p.PersistentVolumeClaimInterface.Create(ctx, intended, options)
	if err != nil {
		return nil, fmt.Errorf("%w: source POST: %w", ErrTargetUncertain, err)
	}
	if err := p.clients.verify(ctx, "PersistentVolumeClaim", created); err != nil {
		return nil, err
	}
	return created, nil
}
func (p *targetClaims) Get(ctx context.Context, name string, options metav1.GetOptions) (*corev1.PersistentVolumeClaim, error) {
	if err := p.clients.verifyCluster(ctx); err != nil {
		return nil, err
	}
	object, err := p.PersistentVolumeClaimInterface.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}
	if err := p.clients.verify(ctx, "PersistentVolumeClaim", object); err != nil {
		return nil, err
	}
	return object, nil
}
func (p *targetClaims) Delete(ctx context.Context, name string, options metav1.DeleteOptions) error {
	object, err := p.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	options, err = p.clients.deletion(ctx, "PersistentVolumeClaim", p.namespace, name, object, options)
	if err != nil {
		return err
	}
	return p.PersistentVolumeClaimInterface.Delete(ctx, name, options)
}

// Settle observes every retained create intent and original-UID-owned child.
// A denied read, replacement or pending POST is uncertainty, never local absence.
// One call performs bounded observations only; the canonical reconciler retries.
func (c *TargetClients) Settle(ctx context.Context, notBefore time.Time) (settled bool, failure error) {
	defer func() {
		if failure != nil && !errors.Is(failure, ErrTargetUncertain) {
			failure = fmt.Errorf("%w: %w", ErrTargetUncertain, failure)
		}
	}()
	if err := c.verifyCluster(ctx); err != nil {
		return false, err
	}
	b := c.journal.Binding()
	rows, err := c.journal.Resources(ctx)
	if err != nil {
		return false, err
	}
	var workflowUID string
	for _, r := range rows {
		if r.Kind == "Workflow" {
			workflowUID = r.UID
		}
	}
	{
		pods, err := c.kube.CoreV1().Pods(b.Namespace).List(ctx, metav1.ListOptions{Limit: 500})
		if err != nil {
			return false, err
		}
		if pods.Continue != "" {
			return false, ErrTargetUncertain
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if workflowUID == "" && possibleTargetChild(p, b.Workflow) {
				if _, found, err := c.resource(ctx, "Pod", b.Namespace, p.Name); err != nil || !found {
					return false, ErrTargetUncertain
				}
			}
			if ownedByTarget(p.OwnerReferences, workflowUID) {
				body, err := json.Marshal(p)
				if err != nil {
					return false, err
				}
				if err := c.journal.RetainOwned(ctx, model.ExecutionTargetResource{RunID: b.RunID, Kind: "Pod", Namespace: b.Namespace, Name: p.Name, UID: string(p.UID), Object: body, NotBefore: notBefore}, workflowUID); err != nil {
					return false, err
				}
			}
		}
		claims, err := c.kube.CoreV1().PersistentVolumeClaims(b.Namespace).List(ctx, metav1.ListOptions{Limit: 500})
		if err != nil {
			return false, err
		}
		if claims.Continue != "" {
			return false, ErrTargetUncertain
		}
		for i := range claims.Items {
			p := &claims.Items[i]
			if workflowUID == "" && possibleTargetChild(p, b.Workflow) {
				if _, found, err := c.resource(ctx, "PersistentVolumeClaim", b.Namespace, p.Name); err != nil || !found {
					return false, ErrTargetUncertain
				}
			}
			if ownedByTarget(p.OwnerReferences, workflowUID) {
				body, err := json.Marshal(p)
				if err != nil {
					return false, err
				}
				if err := c.journal.RetainOwned(ctx, model.ExecutionTargetResource{RunID: b.RunID, Kind: "PersistentVolumeClaim", Namespace: b.Namespace, Name: p.Name, UID: string(p.UID), Object: body, NotBefore: notBefore}, workflowUID); err != nil {
					return false, err
				}
			}
		}
		rows, err = c.journal.Resources(ctx)
		if err != nil {
			return false, err
		}
	}
	allAbsent := true
	for _, r := range rows {
		notBefore = maxTargetTime(notBefore, r.NotBefore)
		var object metav1.Object
		var readErr error
		switch r.Kind {

		case "Workflow":
			object, readErr = c.workflows.Get(ctx, r.Name, metav1.GetOptions{})
		case "Pod":
			object, readErr = c.kube.CoreV1().Pods(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		case "PersistentVolumeClaim":
			object, readErr = c.kube.CoreV1().PersistentVolumeClaims(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		default:
			return false, ErrTargetUncertain
		}
		if apierrors.IsNotFound(readErr) {
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		allAbsent = false
		if object.GetUID() == "" || (r.UID != "" && r.UID != string(object.GetUID())) {
			return false, ErrTargetUncertain
		}
		if r.UID == "" && object.GetAnnotations()[targetBindingAnnotation] != c.digest && !ownedByTarget(object.GetOwnerReferences(), workflowUID) {
			return false, ErrTargetUncertain
		}
		if r.UID == "" {
			if err := c.verify(ctx, r.Kind, object); err != nil {
				return false, err
			}
		}
		uid := object.GetUID()
		options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
		propagation := metav1.DeletePropagationForeground
		options.PropagationPolicy = &propagation
		var deleteErr error
		switch r.Kind {

		case "Workflow":
			deleteErr = c.workflows.Delete(ctx, r.Name, options)
		case "Pod":
			deleteErr = c.kube.CoreV1().Pods(r.Namespace).Delete(ctx, r.Name, options)
		case "PersistentVolumeClaim":
			deleteErr = c.kube.CoreV1().PersistentVolumeClaims(r.Namespace).Delete(ctx, r.Name, options)
		}
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return false, deleteErr
		}
	}
	return allAbsent && !c.now().Before(notBefore), nil
}
func maxTargetTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
func possibleTargetChild(object metav1.Object, workflow string) bool {
	if object.GetLabels()["oberth.ci/workflow"] == workflow || object.GetLabels()["workflows.argoproj.io/workflow"] == workflow {
		return true
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.Name == workflow {
			return true
		}
	}
	return false
}
func ownedByTarget(owners []metav1.OwnerReference, uid string) bool {
	if uid == "" {
		return false
	}
	for _, owner := range owners {
		if owner.Kind == "Workflow" && owner.UID == types.UID(uid) && owner.APIVersion == "argoproj.io/v1alpha1" {
			return true
		}
	}
	return false
}

// VerifyResultCustody retains native result provenance while source delivery
// cleanup is unresolved. Workflow/PVC UIDs must already be durable.
func (c *TargetClients) VerifyResultCustody(ctx context.Context) error {
	if err := c.verifyCluster(ctx); err != nil {
		return err
	}
	cancelled, err := c.journal.Cancelled(ctx)
	if err != nil {
		return err
	}
	if cancelled {
		return ErrTargetUncertain
	}
	b := c.journal.Binding()
	rows, err := c.journal.Resources(ctx)
	if err != nil {
		return err
	}
	workflowFound := false
	for _, r := range rows {
		if r.UID == "" {
			return ErrTargetUncertain
		}
		switch r.Kind {
		case "Workflow":
			workflowFound = true
			if _, err := c.WorkflowClient().Get(ctx, r.Name, metav1.GetOptions{}); err != nil {
				return err
			}
		case "PersistentVolumeClaim":
			if _, err := c.Kubernetes().CoreV1().PersistentVolumeClaims(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{}); err != nil {
				return err
			}
		case "Pod":
			_, err := c.kube.CoreV1().Pods(b.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				if err != nil {
					return err
				}
				return ErrTargetUncertain
			}
		}
	}
	if !workflowFound {
		return ErrTargetUncertain
	}
	return nil
}
