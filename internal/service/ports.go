package service

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type KeyPolicy struct{ Agent bool }
type RefUpdate struct {
	ID, Repo, Ref, PreviousSHA, ObjectSHA, CommitSHA string
	Actor                                            actor.Actor
	Run                                              bool
}
type Decision struct {
	Deletion        *stateport.BranchDeletion
	Request         model.CIRequest
	Release         bool
	ReleaseHead     gitport.Ref
	Action, Message string
}

type RunStore interface {
	RunCounts(context.Context, []string) (map[stepmodel.RunStatus]int, error)
	AcceptBatch(context.Context, stateport.BatchAcceptance) (stateport.AcceptanceResult, error)
	Run(context.Context, string) (stateport.Run, error)
	RunState(context.Context, string) (stateport.RunState, error)
	ResolveRun(context.Context, []string, string) (stateport.Run, error)
	RunByCommit(context.Context, []string, string) (stateport.Run, error)
	RunByRef(context.Context, []string, string) (stateport.Run, error)
	Runs(context.Context, stateport.RunFilter) ([]stateport.Run, error)
	RecentRuns(context.Context, []string) (map[string][]stateport.RunMark, error)
	Nodes(context.Context, string) ([]stateport.Node, error)
	Steps(context.Context, string) ([]stepmodel.RunStep, error)
	Finish(context.Context, stateport.Action, stateport.RunUpdate) error
	Cancel(context.Context, stateport.Action, stateport.RunCancellation) (stateport.Obligation, error)
	Obligations(context.Context, string) ([]stateport.Obligation, error)
	BeginPurge(context.Context, stateport.Action, string, string) error
	FinishPurge(context.Context, stateport.Action, string, string) error
	PurgeReceipt(context.Context, string) (stateport.PurgeReceipt, error)
}
type AuthorityStore interface {
	Current(context.Context, actor.Actor) (actor.Actor, error)
	KeyPolicy(context.Context, actor.Actor) (KeyPolicy, error)
}
type CatalogStore interface {
	RepositoryScope(context.Context, []string) ([]string, error)
	Repository(context.Context, string, []string) (stateport.Repository, error)
	Repositories(context.Context, stateport.CatalogFilter) ([]stateport.Repository, error)
}

// Changes closes after any committed run/event fact, including native progress.
type EventStore interface {
	RecordEvents(context.Context, []stateport.EventWrite) error
	Changes() <-chan struct{}
	Events(context.Context, stateport.EventFilter) (model.EventPage, error)
}

// Source is implemented by wiring over fresh source-delivery metadata.
type Source interface {
	DefaultBranch(context.Context, string) (string, error)
	DefaultFile(context.Context, string, string) (gitport.Ref, gitport.File, error)
}
type Git interface {
	ResolveRef(context.Context, string, string) (gitport.Ref, error)
	ListRefs(context.Context, string) ([]gitport.Ref, error)
	ReleaseTag(context.Context, stateport.Repository, gitport.Ref) (gitport.Ref, error)
}

// Lock is one run's place on one native lock: a holder, a waiter, or, when
// Clear is set, a run that has ended while the workflow controller still counts
// a unit for it; Clear is then the exact command that releases that unit.
type Lock struct {
	Repo, RunID, Name, Clear string
	Steps                    string
	Permits, Held            int
	Limit                    int // Negative when the native ConfigMap could not be inspected.
	Waiting                  bool
}

// Starter is implemented by wiring: prepare, accept and start under its lifetime.
// Request.ReceiveID identifies the stored action and is retained across attempts.
type StartRequest struct {
	Request                      model.CIRequest
	RerunOf, Reason, PromotionID string
}

type Starter interface {
	StartRun(context.Context, StartRequest) (apicontract.Receipt, error)
}
type CheckRequest struct {
	ReleaseTag string
	Request    model.CIRequest
	Files      []apicontract.RunCheckArgsFilesItem
}

type Exec interface {
	Cancel(context.Context, stateport.Run, bool) error
	Inspect(context.Context, CheckRequest) (apicontract.RunCheckResult, error)
	Locks(context.Context) ([]Lock, error)
}

type Logs interface {
	Read(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error)
	Purge(context.Context, stateport.Run) error
}
type eventQuery struct {
	After                   int64
	Limit                   int
	Repos, RunIDs, RefsGlob []string
	Kinds                   []model.EventKind
	Own, Future, Snapshot   bool
	Timeout                 time.Duration
	repositories            []string
}
