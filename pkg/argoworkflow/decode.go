package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

// Decode accepts exactly one bounded native Workflow without duplicate or unknown fields.
func Decode(document []byte) (*wfv1.Workflow, error) {
	if len(document) > admissioncontract.MaxDocumentBytes {
		return nil, refusal("document", "document_size")
	}
	raw, err := oneDocument(document)
	if err != nil {
		return nil, err
	}
	if findings := exactNativeFields(raw, reflect.TypeFor[wfv1.Workflow](), ""); len(findings) > 0 {
		return nil, &AdmissionError{Findings: findings}
	}
	var wf wfv1.Workflow
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wf); err != nil {
		return nil, documentError(err)
	}
	if wf.APIVersion != "argoproj.io/v1alpha1" || wf.Kind != "Workflow" {
		return nil, refusal("apiVersion/kind", "document")
	}
	return &wf, nil
}

func oneDocument(document []byte) ([]byte, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(document))
	var node, extra yamlv3.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, documentError(err)
	}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, refusal("document", "document")
	}
	raw, err := yaml.YAMLToJSONStrict(document)
	if err != nil {
		return nil, documentError(err)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, refusal("document", "document")
	}
	if err := expandedDepth(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func documentError(err error) error {
	return &AdmissionError{Findings: []admissioncontract.Finding{finding("document", "document")}, Detail: err.Error(), cause: err}
}

func expandedDepth(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	depth := 0
	for {
		value, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return documentError(err)
		}
		delimiter, ok := value.(json.Delim)
		if !ok {
			continue
		}
		if delimiter == '{' || delimiter == '[' {
			depth++
		} else {
			depth--
		}
		if depth > admissioncontract.MaxNestingDepth {
			return refusal("document", "depth")
		}
	}
}
