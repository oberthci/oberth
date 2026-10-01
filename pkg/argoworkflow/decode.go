package argoworkflow

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/argoproj/argo-workflows/v4/pkg/apis/workflow"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"sigs.k8s.io/yaml"
)

// APIVersion and Kind are the only group/version/kind this package admits. A
// repository declares a concrete Workflow: WorkflowTemplate and
// ClusterWorkflowTemplate are cluster-scoped named objects an untrusted push
// must not be able to create or reference.
var (
	APIVersion = wfv1.SchemeGroupVersion.Group + "/" + wfv1.SchemeGroupVersion.Version
	Kind       = workflow.WorkflowKind
)

// Decode parses one repository-authored pipeline document into the real
// upstream Argo CRD type.
//
// The decode is strict: an unknown field is an error rather than a silently
// ignored line. That matters because the security gate in Admit reasons about
// named fields — a typo that a lenient decoder would drop must not become an
// unreviewed permission.
func Decode(source []byte) (*wfv1.Workflow, error) {
	if len(source) == 0 || len(bytes.TrimSpace(source)) == 0 {
		return nil, errors.New("argoworkflow: pipeline document is empty")
	}
	if len(source) > MaxSourceBytes {
		return nil, fmt.Errorf("argoworkflow: pipeline document exceeds the %d byte source limit", MaxSourceBytes)
	}
	if err := rejectMultipleDocuments(source); err != nil {
		return nil, err
	}
	var workflow wfv1.Workflow
	if err := yaml.UnmarshalStrict(source, &workflow); err != nil {
		return nil, fmt.Errorf("argoworkflow: decode Workflow: %w", err)
	}
	if workflow.APIVersion != APIVersion {
		return nil, fmt.Errorf("argoworkflow: apiVersion must be %q, got %q", APIVersion, workflow.APIVersion)
	}
	if workflow.Kind != Kind {
		return nil, fmt.Errorf("argoworkflow: kind must be %q, got %q", Kind, workflow.Kind)
	}
	return &workflow, nil
}

// rejectMultipleDocuments refuses a multi-document stream. One file declares
// one pipeline: a second document would be a second, differently-admitted
// object hiding behind a reviewed first one.
//
// YAML document indicators (`---` and `...`) are only recognized at column
// zero (no leading whitespace). An indented `---` or `...` inside a block
// scalar is content, not a document boundary. The earlier version stripped
// leading whitespace before matching, so a literal-block script containing
// `  ---` was rejected as a second document.
//
// `---` after content starts a new document (rejected). `...` ends the
// current document without starting a new one; content after a `...` is an
// implicit second document (rejected). A leading `---` before any content
// opens the first document and is not rejected.
func rejectMultipleDocuments(source []byte) error {
	contentSeen := false
	terminated := false
	for _, line := range strings.Split(string(source), "\n") {
		stripped := strings.TrimRight(line, " \t\r")
		switch stripped {
		case "---":
			if contentSeen {
				return errors.New("argoworkflow: pipeline document must contain exactly one YAML document")
			}
			continue
		case "...":
			if contentSeen {
				terminated = true
			}
			continue
		}
		trimmed := strings.TrimSpace(stripped)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if terminated {
			return errors.New("argoworkflow: pipeline document must contain exactly one YAML document")
		}
		contentSeen = true
	}
	return nil
}
