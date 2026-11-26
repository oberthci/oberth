package stateport

import (
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Run joins one summary to its durable admission, origin and revision.
// Revision fences run-control facts; native observations do not advance it.
type Run struct {
	HasAdmission bool
	Findings     []admissioncontract.Finding
	RepositoryID int64
	stepmodel.RunSummary
	Origin             model.RunOrigin
	Sequence, Revision int64
	Admission          admissioncontract.PreparedView
	EvidenceComplete   bool
	Release            bool
}

// RunFilter applies repository scope before history cursors and metadata decoding.
type RunFilter struct {
	Repositories                           []string
	Before                                 int64
	Cursor, CommitSHA, Ref, RefGlob, Actor string
	Limit                                  int
	Status                                 stepmodel.RunStatus
	Trigger                                model.RunTrigger
	Latest, LatestRepository               bool
	// SkipResumed leaves out resumed runs: selection by commit or ref never
	// picks one; only its own ID names it.
	SkipResumed bool
}

// RunUpdate carries a verdict and its originating catalogue reason.
// Detail retains full native diagnostics; storage never classifies that prose.
type RunUpdate struct {
	ProjectCI      bool
	ID             string
	Revision       int64
	Status         stepmodel.RunStatus
	Failure        *stepmodel.FailureRecord
	Restart        bool
	Publication    *PublicationIntent
	Promotion      *PromotionUpdate
	Reason, Detail string
	NativePhase    *stepmodel.NativePhase
}

const (
	ObligationStop      = "Stop"
	ObligationTerminate = "Terminate"
)

// Obligation persists an exact revision so stale workers cannot clear newer work.
type Obligation struct {
	RunID, Kind, Actor string
	Value              string
	Revision           int64
}

// PurgeReceipt distinguishes durable pending and completed evidence removal.
type PurgeReceipt struct {
	RunID, ID string
	Completed bool
}

type PublicationIntent struct {
	Publication model.Publication
}

// BranchDeletion binds a received deletion to its exact publication intent.
type BranchDeletion struct {
	Request     model.CIRequest
	Publication model.Publication
}

// UnboundFailure records a submission or recovery refusal before Workflow binding.
// Reason is a submission or recovery literal from the status catalogue;
// Detail holds the full caller diagnostic.
type UnboundFailure struct{ RunID, Reason, Detail string }

// Repository is a durable canonical repository identity.
type Repository struct {
	ID                        int64
	Upstream, Name, Canonical string
	Archived                  bool
}

// CatalogFilter uses canonical repository names; nil means all, empty means none.
type CatalogFilter struct {
	Repositories    []string
	Before          int64
	Limit           int
	IncludeArchived bool
}

// RecentRunLimit is how many of a repository's newest runs a repository list shows.
const RecentRunLimit = 20

// RunMark is one run in a repository's recent history: enough to colour it,
// link to it and say what it ran, without its summary or steps.
type RunMark struct {
	ID        string              `json:"id"`
	Status    stepmodel.RunStatus `json:"status"`
	Ref       string              `json:"ref"`
	CreatedAt time.Time           `json:"created_at"`
	// DisplayReason is C2's not-evidence reason for a resumed run, or for a
	// passed assisted one; it is empty otherwise.
	DisplayReason string `json:"display_reason,omitempty"`
	// Origin stays private; C2 derives DisplayReason from it.
	Origin model.RunOrigin `json:"-"`
}

// RunState is the summary-only waiter view; it contains no admission or nodes.
type RunState struct {
	stepmodel.RunSummary
	Revision int64
}

// RunCancellation fences the run revision read during preparation.
type RunCancellation struct {
	ID        string
	Revision  int64
	Terminate bool
}

// Clone detaches the returned projection and immutable admission from the reader.
func (r Run) Clone() Run {
	r.RunSummary = r.RunSummary.Clone()
	r.Admission = r.Admission.Clone()
	r.Findings = slices.Clone(r.Findings)
	return r
}

// ScheduleAdmissionExpiredError marks a digest outside the retained entry window.
type ScheduleAdmissionExpiredError struct{}

func (ScheduleAdmissionExpiredError) Error() string { return "schedule admission expired" }
