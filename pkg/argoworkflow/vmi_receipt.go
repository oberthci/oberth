package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

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
	return b.RunID != "" && b.RepositoryID > 0 && IsFullFragmentSHA(b.SHA) && b.File != "" && b.Trigger.Valid() && ValidateScheduleEntry(b.Trigger, b.ScheduleEntry) == nil
}

// VMIDemandSummary is the durable, server-owned part of the demand receipt.
// The quota annotation seals it with the submitted Workflow's identity. Recovery
// restores that admitted peak rather than re-sizing against a moved fragment tag.
type VMIDemandSummary struct {
	Version      int              `json:"version"`
	Binding      RunSourceBinding `json:"binding"`
	SourceSHA256 string           `json:"source_sha256"`
	GraphSHA256  string           `json:"graph_sha256"`
	Demand       VMIDemand        `json:"demand"`
}

func (s VMIDemandSummary) Valid() bool {
	validHash := func(value string) bool { b, err := hex.DecodeString(value); return err == nil && len(b) == sha256.Size }
	d := s.Demand
	return s.Version == 1 && s.Binding.complete() && validHash(s.SourceSHA256) && validHash(s.GraphSHA256) && d.VMs >= 0 && d.Cores >= 0 && d.MemoryBytes >= 0 && (d.VMs == 0) == (d.Cores == 0) && (d.VMs == 0) == (d.MemoryBytes == 0) && d.Fits(VMIBudgetLimit().DemandLimit())
}

// VMIDemandReceipt cannot be constructed from repository YAML. Fragment bytes
// stay private and are copied at both boundaries, so sizing and submission use
// one exact, policy-admitted resolution even if the source tag subsequently moves.
type VMIDemandReceipt struct {
	summary   VMIDemandSummary
	fragments map[FragmentKey]Fragment
}

func (r *VMIDemandReceipt) Summary() VMIDemandSummary { return r.summary }
func (r *VMIDemandReceipt) Fragments() map[FragmentKey]Fragment {
	return cloneVMIFragments(r.fragments)
}

func cloneVMIFragments(fragments map[FragmentKey]Fragment) map[FragmentKey]Fragment {
	out := make(map[FragmentKey]Fragment, len(fragments))
	for k, f := range fragments {
		f.Source = slices.Clone(f.Source)
		out[k] = f
	}
	return out
}

// ResolvedRunSource is shared by reservation and final submission admission.
// It retains the authored arguments until admission has checked server-owned
// parameter declarations; BindParameters stamps the same authoritative binding
// only after that check, just as ordinary submission has always done.
type ResolvedRunSource struct {
	workflow        *wfv1.Workflow
	lock            Lock
	fragments       map[FragmentKey]Fragment
	identity        VMIDemandSummary
	demandOnce      sync.Once
	cachedDemand    VMIDemand
	cachedDemandErr error
}

func ResolveRunSource(source []byte, fragments map[FragmentKey]Fragment, binding RunSourceBinding) (*ResolvedRunSource, error) {
	wf, err := Decode(source)
	if err != nil {
		return nil, err
	}
	keys, err := FragmentRefs(wf)
	if err != nil {
		return nil, err
	}
	used := make(map[FragmentKey]Fragment, len(keys))
	for _, key := range keys {
		f, ok := fragments[key]
		digest := sha256.Sum256(f.Source)
		if !ok || f.Key != key || !IsFullFragmentSHA(f.SHA) || (IsFullFragmentSHA(key.Version) && key.Version != f.SHA) || (f.Digest != "" && f.Digest != hex.EncodeToString(digest[:])) {
			return nil, fmt.Errorf("argoworkflow: fragment %s has an invalid content/digest binding", key)
		}
		used[key] = f
	}
	lock, err := Resolve(wf, fragments)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Spec    wfv1.WorkflowSpec
		Lock    Lock
		Binding RunSourceBinding
	}{wf.Spec, lock, binding})
	if err != nil {
		return nil, err
	}
	sourceHash, graphHash := sha256.Sum256(source), sha256.Sum256(encoded)
	return &ResolvedRunSource{workflow: wf, lock: lock, fragments: cloneVMIFragments(used), identity: VMIDemandSummary{Version: 1, Binding: binding, SourceSHA256: hex.EncodeToString(sourceHash[:]), GraphSHA256: hex.EncodeToString(graphHash[:])}}, nil
}

func (r *ResolvedRunSource) Workflow() *wfv1.Workflow { return r.workflow.DeepCopy() }
func (r *ResolvedRunSource) Lock() Lock               { return slices.Clone(r.lock) }

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

func (r *ResolvedRunSource) Policy(policy Policy) Policy {
	if r.identity.Binding.complete() {
		policy.runVMISource = r
	}
	return policy
}

// The resolved graph and binding are immutable. Admission may ask repeatedly
// (or concurrently) for the same demand; compute the bounded proof only once.
func (r *ResolvedRunSource) demand() (VMIDemand, error) {
	r.demandOnce.Do(func() { r.cachedDemand, r.cachedDemandErr = r.computeDemand() })
	return r.cachedDemand, r.cachedDemandErr
}

func (r *ResolvedRunSource) computeDemand() (VMIDemand, error) {
	if !r.identity.Binding.complete() {
		return DeclaredVMIDemand(r.workflow)
	}
	sum, err := SummedVMIDemand(r.workflow)
	if err != nil || sum.VMs == 0 {
		return sum, err
	}
	if peak, ok := runVMIPeak(r.workflow, r.identity.Binding.Trigger); ok {
		return peak, nil
	}
	return sum, nil
}

func (r *ResolvedRunSource) demandFor(wf *wfv1.Workflow) (VMIDemand, error) {
	if !reflect.DeepEqual(wf.Spec, r.workflow.Spec) {
		return VMIDemand{}, errors.New("argoworkflow: run sizing context does not match the admitted graph")
	}
	return r.demand()
}

// Receipt admits every resolved template, including otherwise unused fragment
// templates. A hidden resource create is therefore never collapsed as a driver.
func (r *ResolvedRunSource) Receipt(policy Policy) (*VMIDemandReceipt, error) {
	if err := Admit(r.workflow, r.Policy(policy)); err != nil {
		return nil, err
	}
	demand, err := r.demand()
	if err != nil {
		return nil, err
	}
	summary := r.identity
	summary.Demand = demand
	return &VMIDemandReceipt{summary: summary, fragments: cloneVMIFragments(r.fragments)}, nil
}

func (r *VMIDemandReceipt) Verify(other *VMIDemandReceipt) error {
	if r == nil || other == nil || r.summary != other.summary {
		return errors.New("argoworkflow: VMI demand receipt source, trigger or resolved graph changed after reservation")
	}
	return nil
}
