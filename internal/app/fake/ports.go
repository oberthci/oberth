package fake

import (
	"context"
	"sync"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"golang.org/x/crypto/ssh"
)

type Source struct {
	Read      func(context.Context, gitport.Revision, string) (gitport.File, error)
	Export    func(context.Context, string, sourcecontract.Ref) (sourcecontract.Export, error)
	Reachable func(context.Context, sourcecontract.Revision) (int64, bool, error)
}

func (s Source) ReadFile(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
	return s.Read(ctx, r, path)
}
func (s Source) Checkout(ctx context.Context, repo string, ref sourcecontract.Ref) (sourcecontract.Export, error) {
	return s.Export(ctx, repo, ref)
}
func (s Source) Pin(ctx context.Context, r sourcecontract.Revision) (int64, bool, error) {
	return s.Reachable(ctx, r)
}

type Authority struct {
	LoadFunc func(context.Context, app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error)
}

func (a Authority) Load(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
	return a.LoadFunc(ctx, scope)
}

type Native struct {
	Hook          admissioncontract.PrepareHooks
	HookFunc      func(nativebinding.Preparation) admissioncontract.PrepareHooks
	CreateFunc    func(context.Context, *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error)
	FindFunc      func(context.Context, stateport.Run) (nativebinding.RunBinding, bool, error)
	BindFunc      func(context.Context, nativebinding.RunBinding) error
	StopFunc      func(context.Context, nativebinding.RunBinding) error
	TerminateFunc func(context.Context, nativebinding.RunBinding) error
}

func (n Native) Hooks(p nativebinding.Preparation) admissioncontract.PrepareHooks {
	if n.HookFunc != nil {
		return n.HookFunc(p)
	}
	return n.Hook
}
func (n Native) Create(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
	b, err := n.CreateFunc(ctx, p)
	if err == nil {
		err = n.BindFunc(ctx, b)
	}
	return b, err
}
func (n Native) Find(ctx context.Context, r stateport.Run) (nativebinding.RunBinding, bool, error) {
	b, found, err := n.FindFunc(ctx, r)
	if err == nil && found {
		err = n.BindFunc(ctx, b)
	}
	return b, found, err
}
func (n Native) Stop(ctx context.Context, b nativebinding.RunBinding) error {
	return n.StopFunc(ctx, b)
}
func (n Native) Terminate(ctx context.Context, b nativebinding.RunBinding) error {
	return n.TerminateFunc(ctx, b)
}

type Keeper struct {
	mu       sync.Mutex
	bindings map[string][]sourcecontract.Binding
	KeepFunc func(context.Context, string, []sourcecontract.Binding) error
}

func (k *Keeper) Keep(ctx context.Context, id string, bindings []sourcecontract.Binding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if k.KeepFunc != nil {
		if err := k.KeepFunc(ctx, id, bindings); err != nil {
			return err
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.bindings == nil {
		k.bindings = map[string][]sourcecontract.Binding{}
	}
	k.bindings[id] = cloneBindings(bindings)
	return nil
}
func (k *Keeper) Bindings(id string) []sourcecontract.Binding {
	k.mu.Lock()
	defer k.mu.Unlock()
	return cloneBindings(k.bindings[id])
}

type Scanner struct {
	ScanFunc func(context.Context, string, string) (ssh.PublicKey, error)
}

func (s Scanner) Scan(ctx context.Context, address, algorithm string) (ssh.PublicKey, error) {
	return s.ScanFunc(ctx, address, algorithm)
}

// Git uses the shared Git contract; probes consume only RemoteRef.
type Git struct {
	gitport.Git
	Remote func(context.Context, string, string) (gitport.Ref, error)
}

func (g Git) RemoteRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	return g.Remote(ctx, repo, ref)
}

func cloneBindings(bindings []sourcecontract.Binding) []sourcecontract.Binding {
	out := make([]sourcecontract.Binding, len(bindings))
	for i, b := range bindings {
		out[i] = b.Clone()
	}
	return out
}
