package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// KVMPreflightReader exposes only historical evidence, never execution authority.
type KVMPreflightReader interface {
	KVMPreflight(context.Context, string) (vmrunner.KVMExecution, error)
}

type KVMPreflightResponse struct {
	RunID        string                `json:"run_id"`
	RepoID       int64                 `json:"repo_id"`
	Repository   string                `json:"repository"`
	CandidateSHA string                `json:"candidate_sha"`
	ParentStatus model.RunStatus       `json:"parent_status"`
	Present      bool                  `json:"present"`
	Evidence     *KVMPreflightEvidence `json:"evidence,omitempty"`
}

// ReceiptValid describes immutable historical evidence. It says nothing about
// current policy, publication eligibility, or the outcome of the parent run.
type KVMPreflightEvidence struct {
	IntentDigest string               `json:"intent_digest"`
	Intent       vmrunner.KVMIntent   `json:"intent"`
	WorkflowUID  string               `json:"workflow_uid"`
	Submitted    bool                 `json:"submitted"`
	Rejected     bool                 `json:"rejected"`
	Cleaned      bool                 `json:"cleaned"`
	ReceiptValid bool                 `json:"receipt_valid"`
	Receipt      *vmrunner.KVMReceipt `json:"receipt,omitempty"`
	Refusal      *vmrunner.KVMRefusal `json:"refusal,omitempty"`
}

func (service *API) readKVMPreflight(ctx context.Context, id string) (KVMPreflightResponse, error) {
	var result KVMPreflightResponse
	upstreams, ok := service.runs.(upstreamResolver)
	if service.history == nil || service.repositories == nil || service.kvmPreflights == nil || !ok {
		return result, fmt.Errorf("%w: KVM preflight history", ErrUnavailable)
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return result, err
	}
	repo, err := service.repositories.Repository(ctx, run.RepoID)
	if err != nil {
		return result, err
	}
	upstream, err := upstreams.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		return result, err
	}
	if repo.ID != run.RepoID || upstream.ID != repo.UpstreamID {
		return result, fmt.Errorf("%w: invalid KVM repository binding", store.ErrInvalidState)
	}
	result = KVMPreflightResponse{RunID: run.ID, RepoID: run.RepoID,
		Repository: upstream.QualifiedRepo(repo.Name), CandidateSHA: run.TestedSHA, ParentStatus: run.Status}
	if !boundedKVMJSON(result, 4096) {
		return KVMPreflightResponse{}, fmt.Errorf("%w: invalid KVM parent bounds", store.ErrInvalidState)
	}
	execution, err := service.kvmPreflights.KVMPreflight(ctx, run.ID)
	if errors.Is(err, store.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return KVMPreflightResponse{}, err
	}
	// AI-INVARIANT: bind the immutable journal to the resolved run and its current
	// qualified repository before exposing evidence. Cleanup is never a verdict.
	if !validKVMRead(execution, result) {
		return KVMPreflightResponse{}, fmt.Errorf("%w: invalid KVM preflight evidence", store.ErrInvalidState)
	}
	result.Present = true
	result.Evidence = &KVMPreflightEvidence{
		IntentDigest: vmrunner.KVMIntentDigest(execution.Intent), Intent: execution.Intent,
		WorkflowUID: execution.WorkflowUID, Submitted: execution.Submitted, Rejected: execution.Rejected,
		Cleaned: execution.Cleaned, ReceiptValid: execution.Receipt != nil,
		Receipt: execution.Receipt, Refusal: execution.Refusal,
	}
	return result, nil
}

func validKVMRead(value vmrunner.KVMExecution, parent KVMPreflightResponse) bool {
	intent := value.Intent
	if !boundedKVMJSON(intent, 16384) || intent.Validate() != nil ||
		intent.RunID != parent.RunID || intent.RepoID != parent.RepoID ||
		intent.Repository != parent.Repository || intent.CandidateSHA != parent.CandidateSHA ||
		len(value.WorkflowUID) > 128 ||
		(value.WorkflowUID != "" && (!value.Submitted || value.Rejected)) ||
		(value.Rejected && !value.Submitted) ||
		(value.Cleaned && value.Submitted && !value.Rejected && value.WorkflowUID == "") {
		return false
	}
	if receipt := value.Receipt; receipt != nil {
		if !boundedKVMJSON(receipt, 16384) || !value.Submitted || value.Rejected || value.Refusal != nil ||
			value.WorkflowUID == "" || receipt.WorkflowUID != value.WorkflowUID || receipt.Validate(intent) != nil {
			return false
		}
	}
	if refusal := value.Refusal; refusal != nil {
		if !boundedKVMJSON(refusal, 4096) || refusal.Validate() != nil || refusal.ObservedAt.Before(intent.AdmittedAt) ||
			(refusal.Termination != nil && (value.WorkflowUID == "" || refusal.Termination.WorkflowUID != value.WorkflowUID)) {
			return false
		}
	}
	return true
}

func boundedKVMJSON(value any, limit int) bool {
	body, err := json.Marshal(value)
	return err == nil && len(body) <= limit
}
