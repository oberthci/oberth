package argojob

import (
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const podDemandAnnotation = "oberth.ci/declared-pod-demand"
const resourceFallbackAnnotation = "oberth.ci/resource-admission-fallback"

func stampResourceFallback(wf *wfv1.Workflow, reason model.ResourceAdmissionFallback) error {
	delete(wf.Annotations, resourceFallbackAnnotation)
	if !reason.Valid() {
		return errors.New("invalid resource admission fallback")
	}
	if reason != "" {
		wf.Annotations[resourceFallbackAnnotation] = string(reason)
	}
	return nil
}

func readResourceFallback(wf *wfv1.Workflow) (model.ResourceAdmissionFallback, error) {
	r := model.ResourceAdmissionFallback(wf.Annotations[resourceFallbackAnnotation])
	if !r.Valid() {
		return "", errors.New("invalid resource admission fallback binding")
	}
	return r, nil
}

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
	if len(value) > 16<<10 || json.Unmarshal([]byte(value), &d) != nil || !d.Valid() {
		return nil, errors.New("invalid server-declared pod demand binding")
	}
	return &d, nil
}
