package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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

// RunSourceSummary is the durable, server-owned part of the demand receipt.
// Recovery uses the exact source and fragment identities admitted at submission.
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

// DemandReceipt cannot be constructed from repository YAML. Fragment bytes
// stay private and are copied at both boundaries, so sizing and submission use
// one exact, policy-admitted resolution even if the source tag subsequently moves.
type DemandReceipt struct {
	capability bool
	pods       *wfv1.Workflow
	summary    RunSourceSummary
	fragments  map[FragmentKey]Fragment
}

// WorkflowPodDemand reports only pod requests from the bound graph, without
// admission charges or headroom.
func (r *DemandReceipt) WorkflowPodDemand() (*PodDemand, error) {
	if r == nil || r.pods == nil {
		return nil, nil
	}
	return DeclaredCapabilityPeak(r.pods.DeepCopy())
}

// DeclaredPodDemand analyzes the same immutable source and fragment resolution
// whose receipt final submission verifies. No second tag lookup is involved.
func (r *DemandReceipt) DeclaredPodDemand() (*PodDemand, error) {
	if r == nil || r.pods == nil {
		return nil, nil
	}
	if r.capability {
		return DeclaredCapabilityPeak(r.pods.DeepCopy())
	}
	return DeclaredPodPeak(r.pods.DeepCopy())
}

// Workflow returns an isolated copy of the exact admitted source graph.
func (r *DemandReceipt) Workflow() *wfv1.Workflow { return r.pods.DeepCopy() }

func (r *DemandReceipt) Summary() RunSourceSummary { return r.summary }

func (r *DemandReceipt) Fragments() map[FragmentKey]Fragment {
	return cloneSourceFragments(r.fragments)
}

func cloneSourceFragments(fragments map[FragmentKey]Fragment) map[FragmentKey]Fragment {
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
	workflow  *wfv1.Workflow
	lock      Lock
	fragments map[FragmentKey]Fragment
	identity  RunSourceSummary
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
	return &ResolvedRunSource{workflow: wf, lock: lock, fragments: cloneSourceFragments(used), identity: RunSourceSummary{Version: 1, Binding: binding, SourceSHA256: hex.EncodeToString(sourceHash[:]), GraphSHA256: hex.EncodeToString(graphHash[:])}}, nil
}

func (r *ResolvedRunSource) Workflow() *wfv1.Workflow { return r.workflow.DeepCopy() }
func (r *ResolvedRunSource) GraphIdentity() string    { return r.identity.GraphSHA256 }
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

// Receipt admits every resolved template, including otherwise unused fragment
// templates. A hidden resource create is therefore never collapsed as a driver.
func (r *ResolvedRunSource) Receipt(policy Policy) (*DemandReceipt, error) {
	if err := Admit(r.workflow, policy); err != nil {
		return nil, err
	}
	summary := r.identity
	return &DemandReceipt{capability: policy.Capability != nil, summary: summary, fragments: cloneSourceFragments(r.fragments), pods: r.workflow.DeepCopy()}, nil
}

func (r *DemandReceipt) Verify(other *DemandReceipt) error {
	if r == nil || other == nil || r.summary != other.summary || r.capability != other.capability {
		return errors.New("argoworkflow: workflow source receipt source, trigger or resolved graph changed after reservation")
	}
	return nil
}

// HasResourceRequests distinguishes legacy (including limits-only) documents
// from measured or partially requested ones. Partial requests must be completed
// in git. The receipt holds already-resolved fragments.
func (r *DemandReceipt) HasResourceRequests() bool {
	if r == nil || r.pods == nil {
		return false
	}
	declared := false
	check := func(resources corev1.ResourceRequirements) {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if _, ok := resources.Requests[name]; ok {
				declared = true
			}
		}
	}
	var walk func(*wfv1.Template, int)
	walk = func(t *wfv1.Template, depth int) {
		if t == nil || depth > MaxIdentityWalkDepth {
			return
		}
		if t.Container != nil {
			check(t.Container.Resources)
		}
		if t.Script != nil {
			check(t.Script.Resources)
		}
		if t.ContainerSet != nil {
			for _, c := range t.ContainerSet.Containers {
				check(c.Resources)
			}
		}
		for _, c := range t.InitContainers {
			check(c.Resources)
		}
		for _, c := range t.Sidecars {
			check(c.Resources)
		}
		for _, call := range invocations(t) {
			if call.inline != nil {
				walk(call.inline, depth+1)
			}
		}
	}
	walk(r.pods.Spec.TemplateDefaults, 0)
	for i := range r.pods.Spec.Templates {
		walk(&r.pods.Spec.Templates[i], 0)
	}
	return declared
}
