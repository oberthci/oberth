package service

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// PublicationStore commits attempts, delivery, release receipts and observations together.
type PublicationStore interface {
	PublishedRelease(context.Context, string, string) (model.Publication, error)
	Publication(context.Context, string) (model.Publication, error)
	Publications(context.Context, stateport.RecordFilter) ([]model.Publication, error)
	PendingPublications(context.Context, stateport.RecordFilter) ([]model.Publication, error)
	PreparePublication(context.Context, stateport.Action, stateport.PublicationIntent) error
	BindPublication(context.Context, stateport.Action, string, string) (model.Publication, error)
	StartPublicationAttempt(context.Context, stateport.Action, stateport.PublicationAttempt) (model.Publication, error)
	SettlePublication(context.Context, stateport.Action, stateport.PublicationResult) error
	RetryPublication(context.Context, stateport.Action, stateport.PublicationAttempt) error
	AbandonPublication(context.Context, stateport.Action, string) error
}

type PromotionStore interface {
	PreparePromotion(context.Context, stateport.Action, model.Promotion) error
	Promotion(context.Context, string) (model.Promotion, error)
	PromotionForRun(context.Context, string) (model.Promotion, error)
	Promotions(context.Context, stateport.RecordFilter) ([]model.Promotion, error)
	FinishPromotion(context.Context, stateport.Action, stateport.PromotionUpdate) error
}

// MovingGit owns single-source preparation and exact fenced publication/read-back.
type MovingGit interface {
	ObservedRef(context.Context, string, string) (gitport.Ref, error)
	RemoteRef(context.Context, string, string) (gitport.Ref, error)
	PreparePromotion(context.Context, gitport.PromotionRequest) (gitport.Promotion, error)
	Publish(context.Context, model.Publication) (gitport.PublicationReceipt, error)
}

type HoldStore interface {
	Assist(context.Context, stateport.Action, stateport.HoldAction) error
}

// HoldExec is implemented by native execution and logs; service owns authority/audit.
type HoldExec interface {
	List(context.Context, stateport.Run) ([]model.LiveHold, error)
	Abandon(context.Context, stateport.Run, model.LiveHold) error
	Exec(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error)
}

type Timer interface{ Stop() bool }
type AfterFunc func(time.Duration, func()) Timer
