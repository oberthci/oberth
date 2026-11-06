package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/argoproj/argo-workflows/v4/pkg/apis/workflow"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	yamlv3 "go.yaml.in/yaml/v3"
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
func Decode(source []byte) (result *wfv1.Workflow, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &AdmissionError{Cause: resultErr}
		}
	}()
	if len(source) == 0 || len(bytes.TrimSpace(source)) == 0 {
		return nil, errors.New("argoworkflow: pipeline document is empty")
	}
	if len(source) > MaxSourceBytes {
		return nil, fmt.Errorf("argoworkflow: pipeline document exceeds the %d byte source limit", MaxSourceBytes)
	}
	if err := rejectMultipleDocuments(source); err != nil {
		return nil, err
	}
	encoded, err := yaml.YAMLToJSONStrict(source)
	if err != nil {
		return nil, fmt.Errorf("argoworkflow: decode Workflow: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	sources, hasSources := fields["sources"]
	delete(fields, "sources")
	encoded, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var workflow wfv1.Workflow
	if err := yaml.UnmarshalStrict(encoded, &workflow); err != nil {
		return nil, fmt.Errorf("argoworkflow: decode Workflow: %w", err)
	}
	if hasSources {
		if _, exists := workflow.Annotations[SourcesAnnotation]; exists {
			return nil, errors.New("argoworkflow: duplicate sources declaration")
		}
		if _, err := decodePinnedSources(sources); err != nil {
			return nil, err
		}
		if workflow.Annotations == nil {
			workflow.Annotations = map[string]string{}
		}
		workflow.Annotations[SourcesAnnotation] = string(sources)
	}
	if _, err := PinnedSources(&workflow); err != nil {
		return nil, err
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
func rejectMultipleDocuments(source []byte) error {
	decoder := yamlv3.NewDecoder(bytes.NewReader(source))
	var document yamlv3.Node
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("argoworkflow: decode YAML document: %w", err)
	}
	if err := decoder.Decode(&document); !errors.Is(err, io.EOF) {
		return errors.New("argoworkflow: pipeline document must contain exactly one YAML document")
	}
	return nil
}
