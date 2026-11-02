package argojob

import (
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const podDemandAnnotation = "oberth.ci/declared-pod-demand"

func stampPodDemand(workflow *wfv1.Workflow, demand *argoworkflow.PodDemand) {
	delete(workflow.Annotations, podDemandAnnotation)
	if demand != nil {
		body, _ := json.Marshal(demand)
		workflow.Annotations[podDemandAnnotation] = string(body)
	}
}

func readPodDemand(workflow *wfv1.Workflow) (*argoworkflow.PodDemand, error) {
	value := workflow.Annotations[podDemandAnnotation]
	if value == "" {
		return nil, nil
	}
	var d argoworkflow.PodDemand
	if len(value) > 256 || json.Unmarshal([]byte(value), &d) != nil || !d.Valid() {
		return nil, errors.New("invalid server-declared pod demand binding")
	}
	return &d, nil
}
