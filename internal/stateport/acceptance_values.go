package stateport

import (
	"slices"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Acceptance binds the receipt, attributed request and the exact sealed admission.
type Acceptance struct {
	Resume      *model.RunOrigin
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
	// Received records every accepted Git update, including parked/no-pipeline
	// updates. Other actions do not invent receive evidence.
	Received  []model.CIRequest
	Events    []EventWrite
	Runs      []Acceptance
	Deletions []BranchDeletion
}

// ReceivedRef selects the original receive of a run, or the last receive of
// the named ref for sync. ObjectSHA must match the received new object.
type ReceivedRef struct{ Repo, Ref, ObjectSHA, RunID string }

const MaxReceivedRefs = 256

const MaxReceivedHeads = model.MaxReceivedHeads

// InheritReceivedHeads retains only evidence from an immediately preceding
// local receive. The newest replaced head comes first; older heads age out.
func InheritReceivedHeads(old, previousObject string, previous []string) []string {
	heads := []string{old}
	if old != previousObject {
		return heads
	}
	for _, head := range previous {
		if len(heads) == MaxReceivedHeads {
			break
		}
		if !slices.Contains(heads, head) {
			heads = append(heads, head)
		}
	}
	return heads
}

// ValidReceivedRef checks the public ref and both optional complete object IDs.
func ValidReceivedRef(r model.CIRequest) bool {
	if sourcecontract.ValidatePublicRef(r.Ref) != nil {
		return false
	}
	for _, oid := range []string{r.PreviousSHA, r.ObjectSHA} {
		if oid != "" && (sourcecontract.Revision{Repository: r.Repo, Commit: oid}).Validate() != nil {
			return false
		}
	}
	return true
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
