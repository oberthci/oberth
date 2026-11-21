package app

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type Deps struct {
	Lifetime      context.Context
	SourceTimeout time.Duration
	PublicURL     string
	Source        Source
	Authority     Authority
	Service       Service
	Runs          Runs
	Resolver      StartResolver
	Native        Native
	Preparation   nativebinding.Preparation
	Keeper        Keeper
	Deadline      Deadline
	Routes        workflowpolicy.ReleaseRoutes
	SourcePolicy  []byte
	ReleaseFacts  func(context.Context, Request, string) (admissioncontract.ReleaseFacts, error)
}

// Source reads exact objects during its lifetime; Pin proves fresh default ancestry.
type Source interface {
	ReadFile(context.Context, gitport.Revision, string) (gitport.File, error)
	Checkout(context.Context, string, sourcecontract.Ref) (sourcecontract.Export, error)
	Pin(context.Context, sourcecontract.Revision) (repositoryID int64, reachable bool, err error)
}

// Keeper idempotently retains sealed bindings; C6 decides which objects to keep.
type Keeper interface {
	Keep(context.Context, string, []sourcecontract.Binding) error
}
type Scope struct {
	Mode         admissioncontract.Mode
	RepositoryID int64
	nativebinding.Preparation
}

// Authority loads current request evidence and C4 policy for this exact file/tier.
type Authority interface {
	Load(context.Context, Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error)
}

// Native binds Create/Find; only publicerr.Permanent proves a Create refusal final.
type Native interface {
	Hooks(nativebinding.Preparation) admissioncontract.PrepareHooks
	Create(context.Context, *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error)
	Find(context.Context, stateport.Run) (nativebinding.RunBinding, bool, error)
	Stop(context.Context, nativebinding.RunBinding) error
	Terminate(context.Context, nativebinding.RunBinding) error
}
type Request struct {
	RepositoryID      int64
	Repository, RunID string // RunID identifies the admitted run.
	Ref               sourcecontract.Ref
	Trigger           model.RunTrigger
	ScheduleEntry     string
	Release, Assist   bool
	ReleaseHead       gitport.Ref
	Original          *stateport.Run
	Actor             actor.Actor
}
type RefUpdate struct {
	Request      Request
	PreviousSHA  string
	Deleted, Run bool
}
type Receive struct {
	ID      string
	Updates []RefUpdate
	Source  Source
}
type acceptance struct {
	stateport.BatchAcceptance
	Result PushResult
}

// Service decides before admission and records sealed batches without revalidation.
type Service interface {
	Decide(context.Context, service.RefUpdate) (service.Decision, error)
	Accept(context.Context, stateport.BatchAcceptance) (stateport.AcceptanceResult, error)
}

type Runs interface {
	AcceptedRuns(context.Context, string) ([]stateport.Run, error)
	Obligations(context.Context, string) ([]stateport.Obligation, error)
	FailUnbound(context.Context, stateport.Action, stateport.UnboundFailure) error
	UnboundRuns(context.Context, int64, int) ([]stateport.Run, error)
}
type Deadline func(context.Context, time.Duration) (context.Context, context.CancelFunc)

// StartResolver supplies fresh run identity and repository/tier facts for the exact request.
type StartResolver interface {
	ResolveStart(context.Context, service.StartRequest) (Request, error)
}
