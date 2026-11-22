package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/oberthci/oberth/internal/argojob"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type Workflows struct {
	mu                    sync.Mutex
	Object                *wf.Workflow
	CreateError, GetError error
	BeforePatch           func()
	BeforeCreate          func(*wf.Workflow)
	Posts, Gets, Patches  int
}

func (f *Workflows) Create(_ context.Context, w *wf.Workflow, _ meta.CreateOptions) (*wf.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.BeforeCreate != nil {
		f.BeforeCreate(w)
	}
	f.Posts++
	if f.CreateError != nil {
		return nil, f.CreateError
	}
	if f.Object != nil {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "workflows"}, w.Name)
	}
	f.Object = w.DeepCopy()
	f.Object.UID = "workflow-uid"
	f.Object.ResourceVersion = "1"
	return f.Object.DeepCopy(), nil
}
func (f *Workflows) Get(_ context.Context, name string, _ meta.GetOptions) (*wf.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Gets++
	if f.GetError != nil {
		return nil, f.GetError
	}
	if f.Object == nil || f.Object.Name != name {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, name)
	}
	return f.Object.DeepCopy(), nil
}
func (f *Workflows) Patch(_ context.Context, request argojob.PatchRequest) (*wf.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Patches++
	if f.BeforePatch != nil {
		f.BeforePatch()
	}
	p, err := jsonpatch.DecodePatch(request.Body)
	if err != nil {
		return nil, err
	}
	original, err := json.Marshal(f.Object)
	if err != nil {
		return nil, err
	}
	next, err := p.Apply(original)
	if err != nil {
		return nil, err
	}
	var updated wf.Workflow
	if err = json.Unmarshal(next, &updated); err != nil {
		return nil, err
	}
	f.Object = &updated
	return f.Object.DeepCopy(), nil
}

type Accounts struct{ Names map[string]string }

func (f Accounts) Get(_ context.Context, name string, _ meta.GetOptions) (*core.ServiceAccount, error) {
	uid, ok := f.Names[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, name)
	}
	return &core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: name, UID: types.UID(uid)}}, nil
}

// Create records the account with the UID "uid-<name>".
func (f Accounts) Create(_ context.Context, a *core.ServiceAccount, _ meta.CreateOptions) (*core.ServiceAccount, error) {
	if _, ok := f.Names[a.Name]; ok {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "serviceaccounts"}, a.Name)
	}
	f.Names[a.Name] = "uid-" + a.Name
	created := a.DeepCopy()
	created.UID = types.UID(f.Names[a.Name])
	return created, nil
}

type Pods struct {
	Object         *core.Pod
	GetFunc        func() (*core.Pod, error)
	ExecFunc       func(context.Context, argojob.ExecRequest) error
	OnLogs         func()
	Deletes, Opens int
	DeletedUID     types.UID
}

func (f *Pods) Get(_ context.Context, name string, _ meta.GetOptions) (*core.Pod, error) {
	if f.GetFunc != nil {
		return f.GetFunc()
	}
	if f.Object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
	}
	return f.Object.DeepCopy(), nil
}
func (f *Pods) Delete(_ context.Context, _ string, o meta.DeleteOptions) error {
	if o.Preconditions == nil || o.Preconditions.UID == nil || f.Object == nil || *o.Preconditions.UID != f.Object.UID {
		return fmt.Errorf("UID precondition failed")
	}
	if o.Preconditions.ResourceVersion != nil && *o.Preconditions.ResourceVersion != f.Object.ResourceVersion {
		return fmt.Errorf("resource version precondition failed")
	}
	f.Deletes++
	f.DeletedUID = *o.Preconditions.UID
	f.Object = nil
	return nil
}
func (f *Pods) Patch(_ context.Context, request argojob.PatchRequest) (*core.Pod, error) {
	p, err := jsonpatch.DecodePatch(request.Body)
	if err != nil {
		return nil, err
	}
	original, err := json.Marshal(f.Object)
	if err != nil {
		return nil, err
	}
	next, err := p.Apply(original)
	if err != nil {
		return nil, err
	}
	var updated core.Pod
	if err = json.Unmarshal(next, &updated); err != nil {
		return nil, err
	}
	f.Object = &updated
	return f.Object.DeepCopy(), nil
}
func (f *Pods) Exec(ctx context.Context, r argojob.ExecRequest) error {
	if f.ExecFunc != nil {
		return f.ExecFunc(ctx, r)
	}
	return nil
}
func (f *Pods) Logs(context.Context, string, core.PodLogOptions) (io.ReadCloser, error) {
	f.Opens++
	if f.OnLogs != nil {
		f.OnLogs()
	}
	return io.NopCloser(strings.NewReader("container output\n")), nil
}

var _ argojob.Workflows = (*Workflows)(nil)
var _ argojob.Accounts = Accounts{}
var _ argojob.Pods = (*Pods)(nil)
