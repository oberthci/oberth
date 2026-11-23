package contracttest

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

// Function adapters contain no state or validation. Composition fixtures supply
// every callback they exercise, over the real provider or its owner's fake.
type AuthorityFuncs struct {
	CurrentFunc   func(context.Context, actor.Actor) (actor.Actor, error)
	KeyPolicyFunc func(context.Context, actor.Actor) (service.KeyPolicy, error)
}

func (p AuthorityFuncs) Current(ctx context.Context, a actor.Actor) (actor.Actor, error) {
	return p.CurrentFunc(ctx, a)
}
func (p AuthorityFuncs) KeyPolicy(ctx context.Context, a actor.Actor) (service.KeyPolicy, error) {
	return p.KeyPolicyFunc(ctx, a)
}

type GitFuncs struct {
	ResolveRefFunc func(context.Context, string, string) (gitport.Ref, error)
	ListRefsFunc   func(context.Context, string) ([]gitport.Ref, error)
	ReleaseTagFunc func(context.Context, stateport.Repository, gitport.Ref) (gitport.Ref, error)
}

func (p GitFuncs) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	return p.ResolveRefFunc(ctx, repo, ref)
}
func (p GitFuncs) ListRefs(ctx context.Context, repo string) ([]gitport.Ref, error) {
	return p.ListRefsFunc(ctx, repo)
}
func (p GitFuncs) ReleaseTag(ctx context.Context, repo stateport.Repository, ref gitport.Ref) (gitport.Ref, error) {
	return p.ReleaseTagFunc(ctx, repo, ref)
}

type MovingGitFuncs struct {
	ObservedRefFunc      func(context.Context, string, string) (gitport.Ref, error)
	RemoteRefFunc        func(context.Context, string, string) (gitport.Ref, error)
	PreparePromotionFunc func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error)
	PublishFunc          func(context.Context, model.Publication) (gitport.PublicationReceipt, error)
}

func (p MovingGitFuncs) ObservedRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	return p.ObservedRefFunc(ctx, repo, ref)
}

func (p MovingGitFuncs) RemoteRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	return p.RemoteRefFunc(ctx, repo, ref)
}
func (p MovingGitFuncs) PreparePromotion(ctx context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
	return p.PreparePromotionFunc(ctx, q)
}
func (p MovingGitFuncs) Publish(ctx context.Context, q model.Publication) (gitport.PublicationReceipt, error) {
	return p.PublishFunc(ctx, q)
}

type ExecFuncs struct {
	CancelFunc  func(context.Context, stateport.Run, bool) error
	InspectFunc func(context.Context, service.CheckRequest) (apicontract.RunCheckResult, error)
	LocksFunc   func(context.Context) ([]service.Lock, error)
}

func (p ExecFuncs) Cancel(ctx context.Context, r stateport.Run, force bool) error {
	return p.CancelFunc(ctx, r, force)
}
func (p ExecFuncs) Inspect(ctx context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
	return p.InspectFunc(ctx, q)
}
func (p ExecFuncs) Locks(ctx context.Context) ([]service.Lock, error) { return p.LocksFunc(ctx) }

type LogsFuncs struct {
	ReadFunc  func(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error)
	PurgeFunc func(context.Context, stateport.Run) error
}

func (p LogsFuncs) Read(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
	return p.ReadFunc(ctx, r, q)
}
func (p LogsFuncs) Purge(ctx context.Context, r stateport.Run) error { return p.PurgeFunc(ctx, r) }

type HoldExecFuncs struct {
	ListFunc    func(context.Context, stateport.Run) ([]model.LiveHold, error)
	AbandonFunc func(context.Context, stateport.Run, model.LiveHold) error
	ExecFunc    func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error)
}

func (p HoldExecFuncs) List(ctx context.Context, r stateport.Run) ([]model.LiveHold, error) {
	return p.ListFunc(ctx, r)
}
func (p HoldExecFuncs) Abandon(ctx context.Context, r stateport.Run, h model.LiveHold) error {
	return p.AbandonFunc(ctx, r, h)
}
func (p HoldExecFuncs) Exec(ctx context.Context, r stateport.Run, h model.LiveHold, q apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
	return p.ExecFunc(ctx, r, h, q)
}
