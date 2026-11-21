package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	servicecontract "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	clocktest "k8s.io/utils/clock/testing"
)

type testAuthority struct {
	servicecontract.AuthorityFuncs
	Err   error
	reads int
	agent bool
}

func (a *testAuthority) Calls() map[string]int { return map[string]int{"Current": a.reads} }
func newServicePorts(t *testing.T, db serviceStore, wiring service.Starter) service.Config {
	t.Helper()
	clock := clocktest.NewFakeClock(time.Unix(100, 0))
	moving := statefake.NewRuns(clock.Now)
	moving.SeedRepository(stateport.Repository{ID: 1, Canonical: request().Repository})
	ports := unexpectedService(t)
	authority := &testAuthority{}
	authority.AuthorityFuncs = servicecontract.AuthorityFuncs{CurrentFunc: func(ctx context.Context, who actor.Actor) (actor.Actor, error) {
		authority.reads++
		return who.Clone(), errors.Join(authority.Err, ctx.Err())
	}, KeyPolicyFunc: func(ctx context.Context, _ actor.Actor) (service.KeyPolicy, error) {
		return service.KeyPolicy{Agent: authority.agent}, ctx.Err()
	}}
	var sequence atomic.Int64
	return service.Config{Runs: db, Authority: authority, Catalog: db.(service.CatalogStore), Events: db,
		Git: ports.Git, Source: wiring.(service.Source), Exec: ports.Exec, Starter: wiring, Logs: ports.Logs, Now: clock.Now,
		Publications: moving, Promotions: moving, MovingGit: ports.MovingGit, HoldStore: moving, Holds: ports.Holds,
		NewID:     func() string { return fmt.Sprintf("publication-%d", sequence.Add(1)) },
		AfterFunc: func(d time.Duration, f func()) service.Timer { return clock.AfterFunc(d, f) },
		OnError:   func(err error) { t.Error("unexpected publication dispatch", err) }}
}

// Unexpected execution fails the composition test through the owner's adapters.
func unexpectedService(t *testing.T) service.Config {
	t.Helper()
	problem := func() error {
		t.Helper()
		t.Error("unexpected service execution port")
		return errors.New("unexpected service execution port")
	}
	return service.Config{
		Git: servicecontract.GitFuncs{ResolveRefFunc: func(context.Context, string, string) (gitport.Ref, error) { return gitport.Ref{}, problem() }, ListRefsFunc: func(context.Context, string) ([]gitport.Ref, error) { return nil, problem() }, ReleaseTagFunc: func(context.Context, stateport.Repository, gitport.Ref) (gitport.Ref, error) {
			return gitport.Ref{}, problem()
		}},
		MovingGit: servicecontract.MovingGitFuncs{RemoteRefFunc: func(context.Context, string, string) (gitport.Ref, error) { return gitport.Ref{}, problem() }, PreparePromotionFunc: func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error) {
			return gitport.Promotion{}, problem()
		}, PublishFunc: func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
			return gitport.PublicationReceipt{}, problem()
		}},
		Holds: servicecontract.HoldExecFuncs{ListFunc: func(context.Context, stateport.Run) ([]model.LiveHold, error) { return nil, problem() }, AbandonFunc: func(context.Context, stateport.Run, model.LiveHold) error { return problem() }, ExecFunc: func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
			return apicontract.ExecResult{}, problem()
		}},
		Exec: servicecontract.ExecFuncs{CancelFunc: func(context.Context, stateport.Run, bool) error { return problem() }, InspectFunc: func(context.Context, service.CheckRequest) (apicontract.RunCheckResult, error) {
			return apicontract.RunCheckResult{}, problem()
		}, LocksFunc: func(context.Context) ([]service.Lock, error) { return nil, problem() }},
		Logs: servicecontract.LogsFuncs{ReadFunc: func(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error) {
			return apicontract.LogResult{}, problem()
		}, PurgeFunc: func(context.Context, stateport.Run) error { return problem() }},
	}
}
