package nativebinding

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

const (
	RunLabel            = "oberth.ci/run"
	EngineLabel         = "oberth.ci/engine"
	EngineValue         = "argo"
	RepositoryLabel     = "oberth.ci/repo"
	CommitLabel         = "oberth.ci/sha"
	TriggerLabel        = "oberth.ci/trigger"
	TierLabel           = "oberth.ci/tier"
	RunIDAnnotation     = "oberth.ci/run-id"
	RefAnnotation       = "oberth.ci/ref"
	IdentityAnnotation  = "oberth.ci/spec-identity"
	AdmissionAnnotation = "oberth.ci/admission"
	NativeCronLabel     = "workflows.argoproj.io/cron-workflow"
	ManagedLabel        = "app.kubernetes.io/managed-by"
	ManagedValue        = "oberth"
)

// Object identifies one native object, including its replacement-sensitive UID.
type Object struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

func (o Object) valid() bool { return o.Namespace != "" && o.Name != "" && o.UID != "" }

// RunBinding is the immutable ownership record used for settlement and adoption.
// Labels only discover candidates; verification compares this complete record.
type RunBinding struct {
	Workflow     Object                  `json:"workflow"`
	RepositoryID int64                   `json:"repo_id"`
	RunID        string                  `json:"run_id"`
	Revision     sourcecontract.Revision `json:"revision"`
	Ref          string                  `json:"ref"`
	// Trigger is the admitted run trigger (branch, promotion, schedule or tag), not a tier label.
	Trigger string `json:"trigger"`
	// Identity binds the complete sealed admission. SpecSHA256 is the native
	// oberth.ci/spec-identity digest; the store retains the admission authority.
	Identity   string `json:"identity"`
	SpecSHA256 string `json:"spec_sha256"`
}

// Refusal describes a permanent ownership mismatch, never a native API outage.
type Refusal struct {
	reason string
}

func (e *Refusal) Error() string {
	return "native ownership refused: " + e.Reason() + ". " + e.Remedy()
}
func (e *Refusal) Reason() string {
	if e.reason == "" {
		return "ownership proof is incomplete"
	}
	return e.reason
}
func (*Refusal) Remedy() string {
	return "Inspect the persisted binding and the named native object before retrying."
}

// Validate refuses incomplete proof. Admission owns validation of repository,
// revision and trigger vocabulary; this record carries those exact admitted facts.
func (b RunBinding) Validate() error {
	if !b.Workflow.valid() || b.RepositoryID <= 0 || b.RunID == "" || b.Ref == "" || b.Trigger == "" {
		return &Refusal{reason: "Workflow identity or run facts are missing"}
	}
	if b.Revision.Repository == "" || b.Revision.Commit == "" || !digest(b.SpecSHA256) || !digest(b.Identity) {
		return &Refusal{reason: "revision or submitted-spec proof is missing or invalid"}
	}
	return nil
}

// Verify accepts only the identical admitted facts and native UID. Repeating
// verification is harmless; a replacement object requires a new binding.
func (b RunBinding) Verify(observed RunBinding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := observed.Validate(); err != nil {
		return err
	}
	if b != observed {
		return &Refusal{reason: "native object differs from the persisted run binding"}
	}
	return nil
}

// ChildBinding retains verified parent custody after the parent is deleted.
// It records observations; it cannot authorize another Workflow submission.
type ChildBinding struct {
	Run           RunBinding `json:"run"`
	Parent        Object     `json:"parent"`
	ScheduleEntry string     `json:"schedule_entry"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Validate checks the child-specific facts in a complete durable parent proof.
func (b ChildBinding) Validate() error {
	if err := b.Run.Validate(); err != nil {
		return err
	}
	switch {
	case b.Parent.UID == "":
		return &Refusal{reason: "scheduled parent UID is missing"}
	case b.Parent.Namespace != b.Run.Workflow.Namespace:
		return &Refusal{reason: "scheduled parent namespace differs from the child"}
	case b.CreatedAt.IsZero():
		return &Refusal{reason: "scheduled creation time is missing"}
	case b.Run.Trigger != "schedule":
		return &Refusal{reason: "scheduled child requires the schedule run trigger"}
	case b.Run.RunID != b.Run.Workflow.Name:
		return &Refusal{reason: "scheduled child run ID must equal its native name"}
	}
	id, err := CronRepositoryID(b.Parent.Name, b.ScheduleEntry)
	if err != nil {
		return err
	}
	if id != b.Run.RepositoryID {
		return &Refusal{reason: "scheduled repository differs from its parent"}
	}
	if !ChildName(b.Run.Workflow.Name, b.Parent.Name) {
		return &Refusal{reason: "scheduled child name has no canonical positive decimal suffix"}
	}
	return nil
}

// Verify refuses drift from the recorded child/parent proof, including creation time.
func (b ChildBinding) Verify(observed ChildBinding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := observed.Validate(); err != nil {
		return err
	}
	if b.Run != observed.Run || b.Parent != observed.Parent || b.ScheduleEntry != observed.ScheduleEntry || !b.CreatedAt.Equal(observed.CreatedAt) {
		return &Refusal{reason: "scheduled child differs from its durable parent proof"}
	}
	return nil
}

func digest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

// CronName is deterministic and fits the native 52-byte parent-name bound.
func CronName(repositoryID int64, entry string) (string, error) {
	if repositoryID <= 0 || entry == "" {
		return "", &Refusal{reason: "schedule identity requires a positive repository ID and entry"}
	}
	return cronName(repositoryID, entry), nil
}
