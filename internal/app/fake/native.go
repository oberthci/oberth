package fake

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

type nativeObjects struct {
	mu      sync.Mutex
	objects map[string]nativebinding.RunBinding
	owners  map[string]string
	read    func(context.Context, string) (stateport.Run, error)
}

// NewNative retains a fixed-name object and persists binding on Create and Find.
func NewNative(read func(context.Context, string) (stateport.Run, error), bind func(context.Context, nativebinding.RunBinding) error) Native {
	objects := &nativeObjects{objects: map[string]nativebinding.RunBinding{}, owners: map[string]string{}, read: read}
	return Native{BindFunc: bind, CreateFunc: objects.create, FindFunc: objects.find, StopFunc: objects.stop, TerminateFunc: objects.stop}
}
func (n *nativeObjects) create(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
	r, err := n.read(ctx, p.View().RunID)
	if err != nil {
		return nativebinding.RunBinding{}, err
	}
	v := p.View()
	name := nativebinding.WorkflowName(v.RunID)
	key := v.Identity.Namespace + "/" + name
	n.mu.Lock()
	defer n.mu.Unlock()
	if old, ok := n.objects[key]; ok {
		if n.owners[key] != nativeOwner(v) {
			return nativebinding.RunBinding{}, &nativebinding.Refusal{}
		}
		return old, nil
	}
	b := nativebinding.RunBinding{Workflow: nativebinding.Object{Namespace: v.Identity.Namespace, Name: name, UID: "uid-" + v.RunID}, RepositoryID: r.RepositoryID, RunID: v.RunID, Revision: sourcecontract.Revision{Repository: v.Repository, Commit: v.CommitSHA}, Ref: v.Ref, Trigger: string(v.Trigger), Identity: v.Digest, SpecSHA256: v.SpecDigest}
	n.objects[key], n.owners[key] = b, nativeOwner(v)
	return b, nil
}
func (n *nativeObjects) find(ctx context.Context, r stateport.Run) (nativebinding.RunBinding, bool, error) {
	if err := ctx.Err(); err != nil {
		return nativebinding.RunBinding{}, false, err
	}
	key := r.Admission.Identity.Namespace + "/" + nativebinding.WorkflowName(r.ID)
	n.mu.Lock()
	defer n.mu.Unlock()
	b, ok := n.objects[key]
	if ok && n.owners[key] != nativeOwner(r.Admission) {
		return nativebinding.RunBinding{}, false, &nativebinding.Refusal{}
	}
	return b, ok, nil
}
func (n *nativeObjects) stop(ctx context.Context, b nativebinding.RunBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if got, ok := n.objects[b.Workflow.Namespace+"/"+b.Workflow.Name]; !ok || got != b {
		return &nativebinding.Refusal{}
	}
	return nil
}
func nativeOwner(v admissioncontract.PreparedView) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
