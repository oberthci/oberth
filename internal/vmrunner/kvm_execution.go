package vmrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// KVMRecipe is administrator-owned. Hashes are an independent allowlist for
// executable suite code; tests.yaml can select, but never create, this authority.
type KVMRecipe struct {
	SuiteSHA256  string `json:"suite_sha256"`
	GuestSHA256  string `json:"guest_sha256"`
	HarnessImage string `json:"harness_image"`
	SuiteURL     string `json:"suite_url"`
	GuestURL     string `json:"guest_url"`
}

func (r KVMRecipe) Validate() error {
	if !sha256HexPattern.MatchString(r.SuiteSHA256) || !sha256HexPattern.MatchString(r.GuestSHA256) || len(validateGuestImageRef(r.HarnessImage)) != 0 {
		return errors.New("trusted recipe requires complete suite, guest and harness pins")
	}
	for _, raw := range []string{r.SuiteURL, r.GuestURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return errors.New("trusted recipe requires credential-free HTTPS bundle endpoints")
		}
	}
	return nil
}

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
	if i.RunID == "" || i.RepoID <= 0 || i.Repository == "" || !hexSHA1Pattern.MatchString(i.CandidateSHA) || !hexSHA1Pattern.MatchString(i.BaselineSHA) || ValidateContract(i.Contract) != nil || i.Contract.Version != 2 || i.Recipe.Validate() != nil || i.Contract.SuiteSHA256 != i.Recipe.SuiteSHA256 || i.Contract.GuestSHA256 != i.Recipe.GuestSHA256 || i.Namespace == "" || i.WorkflowName == "" || !sha256HexPattern.MatchString(i.WorkflowSpecDigest) || !sha256HexPattern.MatchString(i.InventoryDigest) || i.CaseCount < 1 || i.CaseCount > MaxSuiteCases || i.AdmittedAt.IsZero() || !i.Deadline.After(i.AdmittedAt) || i.Deadline.Sub(i.AdmittedAt) > MaxDeadline {
		return errors.New("invalid trusted KVM preflight intent")
	}
	return nil
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

type KVMExecution struct {
	Intent      KVMIntent
	WorkflowUID string
	Submitted   bool
	Rejected    bool
	Cleaned     bool
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
	CompleteKVMPreflightCleanup(context.Context, string, string) error
	KVMPreflight(context.Context, string) (KVMExecution, error)
	PendingKVMPreflights(context.Context) ([]KVMExecution, error)
}

func (s VerifiedKVMSuite) Inventory() (string, int) {
	results := make([]KVMCaseResult, len(s.caseIDs))
	for n, id := range s.caseIDs {
		results[n] = KVMCaseResult{ID: id, Outcome: "passed"}
	}
	b, _ := json.Marshal(results)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), len(results)
}
func (s VerifiedKVMSuite) CaseIDs() []string { return append([]string(nil), s.caseIDs...) }
