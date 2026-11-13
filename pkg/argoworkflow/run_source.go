package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// RunSourceBinding comes exclusively from the durable run and server-selected
// file, never from document parameters, defaults, environment or planner output.
// An incomplete binding earns no run-aware lifetime credits.
type RunSourceBinding struct {
	RunID         string     `json:"run_id"`
	RepositoryID  int64      `json:"repository_id"`
	SHA           string     `json:"sha"`
	File          string     `json:"file"`
	Trigger       RunTrigger `json:"trigger"`
	ScheduleEntry string     `json:"schedule_entry,omitempty"`
}

func (b RunSourceBinding) complete() bool {
	return b.RunID != "" && b.RepositoryID > 0 && isFullCommitSHA(b.SHA) && b.File != "" && b.Trigger.Valid() && ValidateScheduleEntry(b.Trigger, b.ScheduleEntry) == nil
}

func isFullCommitSHA(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 20
}

// RunSourceSummary is the durable, server-owned part of the source receipt.
// Recovery uses the exact source identity admitted at submission.
type RunSourceSummary struct {
	Version      int              `json:"version"`
	Binding      RunSourceBinding `json:"binding"`
	SourceSHA256 string           `json:"source_sha256"`
	GraphSHA256  string           `json:"graph_sha256"`
}

func (s RunSourceSummary) Valid() bool {
	validHash := func(value string) bool { b, err := hex.DecodeString(value); return err == nil && len(b) == sha256.Size }
	return s.Version == 1 && s.Binding.complete() && validHash(s.SourceSHA256) && validHash(s.GraphSHA256)
}

// SourceReceipt holds an isolated copy of the exact admitted source graph.
type SourceReceipt struct {
	capability bool
	pods       *wfv1.Workflow
	summary    RunSourceSummary
}

// Workflow returns an isolated copy of the exact admitted source graph.
func (r *SourceReceipt) Workflow() *wfv1.Workflow { return r.pods.DeepCopy() }

func (r *SourceReceipt) Summary() RunSourceSummary { return r.summary }

// ResolvedRunSource is shared by reservation and final submission admission.
// It retains the authored arguments until admission has checked server-owned
// parameter declarations; BindParameters stamps the same authoritative binding
// only after that check, just as ordinary submission has always done.
type ResolvedRunSource struct {
	workflow *wfv1.Workflow
	identity RunSourceSummary
}

func ResolveRunSource(source []byte, binding RunSourceBinding) (*ResolvedRunSource, error) {
	wf, err := Decode(source)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Spec    wfv1.WorkflowSpec
		Binding RunSourceBinding
	}{wf.Spec, binding})
	if err != nil {
		return nil, err
	}
	sourceHash, graphHash := sha256.Sum256(source), sha256.Sum256(encoded)
	return &ResolvedRunSource{workflow: wf, identity: RunSourceSummary{Version: 1, Binding: binding, SourceSHA256: hex.EncodeToString(sourceHash[:]), GraphSHA256: hex.EncodeToString(graphHash[:])}}, nil
}

func (r *ResolvedRunSource) Workflow() *wfv1.Workflow { return r.workflow.DeepCopy() }

// BindParameters uses the identical run binding used for sizing. Legacy
// trusted callers may derive a fallback from their tier; YAML never supplies it.
func (r *ResolvedRunSource) BindParameters(wf *wfv1.Workflow, fallback RunTrigger) error {
	trigger := r.identity.Binding.Trigger
	if trigger == "" {
		trigger = fallback
	}
	if err := SetTriggerParameter(wf, trigger); err != nil {
		return err
	}
	return SetScheduleParameter(wf, trigger, r.identity.Binding.ScheduleEntry)
}

// Receipt admits every authored template, including otherwise unused templates.
func (r *ResolvedRunSource) Receipt(policy Policy) (*SourceReceipt, error) {
	if err := Admit(r.workflow, policy); err != nil {
		return nil, err
	}
	summary := r.identity
	return &SourceReceipt{capability: policy.Capability != nil, summary: summary, pods: r.workflow.DeepCopy()}, nil
}

func (r *SourceReceipt) Verify(other *SourceReceipt) error {
	if r == nil || other == nil || r.summary != other.summary || r.capability != other.capability {
		return errors.New("argoworkflow: workflow source receipt source, trigger or resolved graph changed before submission")
	}
	return nil
}
