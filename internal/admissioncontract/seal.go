package admissioncontract

import (
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

// SealError identifies an incomplete or unencodable prepared result. It carries
// no authority and remains discoverable through wrapping and joined errors.
type SealError struct {
	Path   string
	reason string
	detail string
}

func (e *SealError) Error() string {
	return "prepared " + e.Path + ": " + e.Reason() + ". " + e.Remedy() + " " + e.detail
}

func (e *SealError) Reason() string {
	if e.reason == "" {
		return "prepared result is incomplete"
	}
	return e.reason
}

func (e *SealError) Remedy() string {
	switch e.Path {
	case "workflow":
		return "Prepare a submitted Workflow before sealing."
	case "seal":
		return "Use the result returned by preparation."
	case "child":
		return "Check that the Workflow was created by its Oberth schedule."
	}
	return "Correct the JSON snapshot and prepare the Workflow again."
}

// Seal may be called only by pkg/argoworkflow; the dependency guard enforces this symbol rule.
// It clones inputs, sorts template snapshots by namespace/name/UID/version,
// and computes both digests. Missing Workflow and unencodable JSON are refused.
// C4 validates policy, identities, grants, status and plan bounds before sealing.
// Supplied digest strings are replaced. Callers cannot mutate the sealed value.
func Seal(input PreparedView) (*PreparedAdmission, error) {
	view := input
	if view.Workflow == nil {
		return nil, &SealError{Path: "workflow", reason: "submitted Workflow is required"}
	}
	// Encode before cloning: native Item copying assumes valid JSON.
	view.Templates = slices.Clone(view.Templates)
	slices.SortFunc(view.Templates, compareTemplates)
	spec, err := sourcecontract.CanonicalJSON(view.Workflow.Spec)
	if err != nil {
		return nil, sealError("workflow.spec", err)
	}
	view.SpecDigest = sourcecontract.Digest(spec)
	bound, err := digestView(view)
	if err != nil {
		return nil, sealError("view", err)
	}
	view.Digest = sourcecontract.Digest(bound)
	return &PreparedAdmission{view: view.Clone()}, nil
}

// View returns a deep copy for audit, source intent, explanation and Create.
// A zero PreparedAdmission has an empty view and cannot be marshaled as sealed.
func (p PreparedAdmission) View() PreparedView { return p.view.Clone() }

// MarshalJSON emits canonical prepared view JSON. The zero value refuses to
// masquerade as a sealed result; decoded JSON cannot construct a seal.
func (p PreparedAdmission) MarshalJSON() ([]byte, error) {
	if p.view.Digest == "" {
		return nil, &SealError{Path: "seal", reason: "result is not sealed"}
	}
	return sourcecontract.CanonicalJSON(p.view)
}

func digestView(v PreparedView) ([]byte, error) {
	// The shallower field hides only Digest, including any caller-supplied value.
	return sourcecontract.CanonicalJSON(struct {
		PreparedView
		Digest string `json:"digest,omitempty"`
	}{PreparedView: v})
}

func sealError(path string, err error) error {
	return &SealError{Path: path, reason: "Prepared JSON cannot be encoded", detail: err.Error()}
}

func compareTemplates(a, b TemplateSnapshot) int {
	for _, pair := range [][2]string{{a.Namespace, b.Namespace}, {a.Name, b.Name}, {a.UID, b.UID}, {a.ResourceVersion, b.ResourceVersion}} {
		if comparison := strings.Compare(pair[0], pair[1]); comparison != 0 {
			return comparison
		}
	}
	return 0
}
