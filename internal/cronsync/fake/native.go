// Package fake provides detached, context-aware in-memory schedule ports.
// Spec: docs/plans/c10-schedules.md#inspections
package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type Hook func(context.Context, string, string) error

// Action retains mutation preconditions independently of the caller's objects.
type Action struct {
	Verb, Name string
	Delete     metav1.DeleteOptions
}
type Native struct {
	mu        sync.Mutex
	namespace string
	now       func() time.Time
	hook      Hook
	objects   map[string]*wfv1.CronWorkflow
	sequence  int
	actions   []Action
	children  map[types.UID]*wfv1.Workflow
}

func NewNative(namespace string, now func() time.Time, hook Hook) *Native {
	return &Native{namespace: namespace, now: now, hook: hook, objects: map[string]*wfv1.CronWorkflow{}, children: map[types.UID]*wfv1.Workflow{}}
}

// TrackChild connects the native child to garbage-collection owner propagation.
func (n *Native) TrackChild(child *wfv1.Workflow) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.children[child.UID] = child
}
func resource() schema.GroupResource {
	return schema.GroupResource{Group: wfv1.SchemeGroupVersion.Group, Resource: "cronworkflows"}
}
func (n *Native) before(ctx context.Context, verb, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.hook != nil {
		if err := n.hook(ctx, verb, name); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (n *Native) Get(ctx context.Context, name string, _ metav1.GetOptions) (*wfv1.CronWorkflow, error) {
	if err := n.before(ctx, "get", name); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if object := n.objects[name]; object != nil {
		return object.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(resource(), name)
}
func (n *Native) List(ctx context.Context, options metav1.ListOptions) (*wfv1.CronWorkflowList, error) {
	if err := n.before(ctx, "list", ""); err != nil {
		return nil, err
	}
	selector, err := labels.Parse(options.LabelSelector)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	result := &wfv1.CronWorkflowList{}
	for _, name := range slices.Sorted(maps.Keys(n.objects)) {
		object := n.objects[name]
		if selector.Matches(labels.Set(object.Labels)) {
			result.Items = append(result.Items, *object.DeepCopy())
		}
	}
	return result, nil
}
func (n *Native) Create(ctx context.Context, object *wfv1.CronWorkflow, _ metav1.CreateOptions) (*wfv1.CronWorkflow, error) {
	if err := n.before(ctx, "create", object.Name); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.objects[object.Name] != nil {
		return nil, apierrors.NewAlreadyExists(resource(), object.Name)
	}
	if object.Namespace != n.namespace {
		return nil, apierrors.NewForbidden(resource(), object.Name, fmt.Errorf("namespace differs"))
	}
	n.sequence++
	next := object.DeepCopy()
	next.UID = types.UID("parent-" + strconv.Itoa(n.sequence))
	next.ResourceVersion = strconv.Itoa(n.sequence)
	next.CreationTimestamp = metav1.NewTime(n.now())
	n.objects[next.Name] = next
	n.actions = append(n.actions, Action{Verb: "create", Name: next.Name})
	return next.DeepCopy(), nil
}
func (n *Native) Update(ctx context.Context, object *wfv1.CronWorkflow, _ metav1.UpdateOptions) (*wfv1.CronWorkflow, error) {
	if err := n.before(ctx, "update", object.Name); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	old := n.objects[object.Name]
	if old == nil {
		return nil, apierrors.NewNotFound(resource(), object.Name)
	}
	if object.Namespace != n.namespace || object.UID != old.UID || object.ResourceVersion != old.ResourceVersion {
		return nil, apierrors.NewConflict(resource(), object.Name, fmt.Errorf("identity or version differs"))
	}
	n.sequence++
	next := object.DeepCopy()
	next.ResourceVersion = strconv.Itoa(n.sequence)
	n.objects[next.Name] = next
	n.actions = append(n.actions, Action{Verb: "update", Name: next.Name})
	return next.DeepCopy(), nil
}
func (n *Native) Delete(ctx context.Context, name string, options metav1.DeleteOptions) error {
	if err := n.before(ctx, "delete", name); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	old := n.objects[name]
	if old == nil {
		return apierrors.NewNotFound(resource(), name)
	}
	if options.Preconditions != nil && options.Preconditions.UID != nil && *options.Preconditions.UID != old.UID {
		return apierrors.NewConflict(resource(), name, fmt.Errorf("UID differs"))
	}
	delete(n.objects, name)
	if options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationOrphan {
		for _, child := range n.children {
			before := len(child.OwnerReferences)
			child.OwnerReferences = slices.DeleteFunc(child.OwnerReferences, func(owner metav1.OwnerReference) bool { return owner.UID == old.UID })
			if len(child.OwnerReferences) != before {
				n.sequence++
				child.ResourceVersion = strconv.Itoa(n.sequence)
			}
		}
	}
	n.actions = append(n.actions, Action{Verb: "delete", Name: name, Delete: *options.DeepCopy()})
	return nil
}
func (n *Native) Actions() []Action {
	n.mu.Lock()
	defer n.mu.Unlock()
	result := slices.Clone(n.actions)
	for i := range result {
		result[i].Delete = *result[i].Delete.DeepCopy()
	}
	return result
}
