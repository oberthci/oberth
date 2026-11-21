package stateport

import (
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Acceptance binds the receipt, attributed request and the exact sealed admission.
type Acceptance struct {
	Rejected    *RejectedAdmission
	Actions     []model.AuditAction
	Request     model.CIRequest
	Prepared    *admissioncontract.PreparedAdmission
	Release     bool
	RerunOf     string
	PromotionID string
	Child       *nativebinding.ChildBinding
	Events      []EventWrite
}

// BatchAcceptance commits every admitted or rejected run of one receive together.
// Repository and Actor identify the receipt even when the push creates no runs.
type BatchAcceptance struct {
	ReceiveID  string
	Repository string
	Actor      actor.Actor
	Events     []EventWrite
	Runs       []Acceptance
	Deletions  []BranchDeletion
}

// RejectedAdmission retains public findings without inventing an execution plan.
type RejectedAdmission struct {
	ID       string
	Failure  stepmodel.FailureRecord
	Findings []admissioncontract.Finding
}

// AcceptanceResult returns ordered stored outcomes and whether the receipt is new.
// Replays return their recorded outcomes with Fresh false; failures return zero.
type AcceptanceResult struct {
	Runs  []Run
	Fresh bool
}
