package vmrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/protectedtests"
)

type KVMRecipe = protectedtests.Recipe

// KVMIntent precedes the credential-free preflight Workflow. The parent run has
// no Job name yet: release credentials cannot be delivered until cleanup/gate.
type KVMIntent struct {
	RunID              string    `json:"run_id"`
	RepoID             int64     `json:"repo_id"`
	Repository         string    `json:"repository"`
	CandidateSHA       string    `json:"candidate_sha"`
	BaselineSHA        string    `json:"baseline_sha"`
	Contract           Contract  `json:"contract"`
	Recipe             KVMRecipe `json:"recipe"`
	Namespace          string    `json:"namespace"`
	WorkflowName       string    `json:"workflow_name"`
	WorkflowSpecDigest string    `json:"workflow_spec_digest"`
	InventoryDigest    string    `json:"inventory_digest"`
	CaseCount          int       `json:"case_count"`
	AdmittedAt         time.Time `json:"admitted_at"`
	Deadline           time.Time `json:"deadline"`
}

func (i KVMIntent) Validate() error {
	_, descriptorErr := i.Descriptor()
	if i.RunID == "" || i.RepoID <= 0 || i.Repository == "" || !hexSHA1Pattern.MatchString(i.CandidateSHA) || !hexSHA1Pattern.MatchString(i.BaselineSHA) || descriptorErr != nil || i.Namespace == "" || i.WorkflowName == "" || !sha256HexPattern.MatchString(i.WorkflowSpecDigest) || !sha256HexPattern.MatchString(i.InventoryDigest) || i.CaseCount < 1 || i.CaseCount > MaxSuiteCases || i.AdmittedAt.IsZero() || !i.Deadline.After(i.AdmittedAt) || i.Deadline.Sub(i.AdmittedAt) > MaxDeadline {
		return errors.New("invalid trusted KVM preflight intent")
	}
	return nil
}

// Descriptor projects an existing journal record without changing its format.
// This validates binding only; current recipe approval is checked separately.
func (i KVMIntent) Descriptor() (protectedtests.Descriptor, error) {
	return protectedtests.BindContract(i.Contract, i.Recipe)
}
func KVMIntentDigest(intent KVMIntent) string {
	b, _ := json.Marshal(intent)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// KVMReport is emitted only by the pinned host harness, never by the guest.
// Compact digests fit kubelet's 4KiB termination-message limit at max inventory.
type KVMReport struct {
	Version           int    `json:"version"`
	IntentDigest      string `json:"intent_digest"`
	InventoryDigest   string `json:"inventory_digest"`
	ArtifactSetDigest string `json:"artifact_set_digest"`
	CaseCount         int    `json:"case_count"`
}
type KVMTerminationRecord struct {
	ImageID      string    `json:"image_id"`
	ContainerID  string    `json:"container_id"`
	PodUID       string    `json:"pod_uid"`
	WorkflowUID  string    `json:"workflow_uid"`
	ExitCode     int32     `json:"exit_code"`
	Signal       int32     `json:"signal"`
	RestartCount int32     `json:"restart_count"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

func (r KVMTerminationRecord) Validate(binding KVMTerminationBinding) error {
	expected := binding.HarnessImageRef[strings.LastIndex(binding.HarnessImageRef, "@")+1:]
	if r.ImageID == "" || runtimeImageDigest(r.ImageID) != expected || r.ContainerID == "" || r.ContainerID != binding.ContainerID || r.PodUID == "" || r.PodUID != binding.PodUID || r.WorkflowUID == "" || r.WorkflowUID != binding.WorkflowUID || r.ExitCode != 0 || r.Signal != 0 || r.RestartCount != 0 || r.StartedAt.IsZero() || r.FinishedAt.IsZero() || r.StartedAt.Before(binding.AdmittedAt) || r.FinishedAt.Before(r.StartedAt) || r.FinishedAt.After(binding.Deadline) {
		return errors.New("invalid journaled kubelet termination record")
	}
	return nil
}

type KVMReceipt struct {
	Termination KVMTerminationRecord `json:"termination"`

	IntentDigest string                `json:"intent_digest"`
	WorkflowUID  string                `json:"workflow_uid"`
	Binding      KVMTerminationBinding `json:"binding"`
	Report       KVMReport             `json:"report"`
	FinishedAt   time.Time             `json:"finished_at"`
}

func (r KVMReceipt) Validate(intent KVMIntent) error {
	if intent.Validate() != nil || r.Termination.Validate(r.Binding) != nil || !r.Termination.FinishedAt.Equal(r.FinishedAt) || r.IntentDigest != KVMIntentDigest(intent) || r.WorkflowUID == "" || r.Binding.WorkflowUID != r.WorkflowUID || r.Binding.WorkflowName != intent.WorkflowName || r.Binding.Namespace != intent.Namespace || r.Binding.PodName == "" || r.Binding.PodUID == "" || r.Binding.ContainerID == "" || r.Binding.HarnessImageRef != intent.Recipe.HarnessImage || !r.Binding.AdmittedAt.Equal(intent.AdmittedAt) || !r.Binding.Deadline.Equal(intent.Deadline) || r.FinishedAt.Before(intent.AdmittedAt) || r.FinishedAt.After(intent.Deadline) || r.Report.Version != 1 || r.Report.IntentDigest != r.IntentDigest || r.Report.InventoryDigest != intent.InventoryDigest || r.Report.CaseCount != intent.CaseCount || !sha256HexPattern.MatchString(r.Report.ArtifactSetDigest) {
		return errors.New("trusted KVM receipt does not match immutable admission")
	}
	return nil
}

// KVMRefusal is bounded server evidence of a failed preflight, never authority
// to deliver or publish. Candidate logs and free-form errors are not journaled.
type KVMRefusal struct {
	Code        string                `json:"code"`
	ObservedAt  time.Time             `json:"observed_at"`
	Termination *KVMTerminationRecord `json:"termination,omitempty"`
}

func (r KVMRefusal) Validate() error {
	switch r.Code {
	case "preflight_failed", "create_refused", "observation_refused", "canceled", "deadline":
	default:
		return errors.New("invalid trusted refusal code")
	}
	if r.ObservedAt.IsZero() {
		return errors.New("trusted refusal requires observation time")
	}
	if r.Termination != nil && (len(r.Termination.ImageID) > 1024 || len(r.Termination.ContainerID) > 512 || len(r.Termination.PodUID) > 128 || len(r.Termination.WorkflowUID) > 128) {
		return errors.New("trusted refusal evidence exceeds bounds")
	}
	return nil
}

type KVMExecution struct {
	Intent      KVMIntent
	WorkflowUID string
	Submitted   bool
	Rejected    bool
	Cleaned     bool
	Refusal     *KVMRefusal
	Receipt     *KVMReceipt
}

// KVMJournal shares vm_executions and capacity with existing VM execution.
// An uncertain create stays submitted and keeps capacity until exact UID cleanup.
type KVMJournal interface {
	ReserveKVMPreflight(context.Context, KVMIntent) (KVMExecution, error)
	SubmitKVMPreflight(context.Context, string) error
	BindKVMPreflight(context.Context, string, string) error
	RejectKVMPreflight(context.Context, string) error
	RecordKVMReceipt(context.Context, string, KVMReceipt) error
	RecordKVMRefusal(context.Context, string, KVMRefusal) error
	CompleteKVMPreflightCleanup(context.Context, string, string) error
	KVMPreflight(context.Context, string) (KVMExecution, error)
	PendingKVMPreflights(context.Context) ([]KVMExecution, error)
}
