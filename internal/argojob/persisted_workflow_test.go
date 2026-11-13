package argojob

import (
	"context"
	"encoding/json"
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type persistedWorkflowClient struct {
	existing *wfv1.Workflow
	creates  int
}

func (client *persistedWorkflowClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	if client.existing == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, name)
	}
	return client.existing.DeepCopy(), nil
}
func (client *persistedWorkflowClient) Create(_ context.Context, workflow *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	client.creates++
	// Model API serialization and ordinary persisted metadata/status; do not
	// accidentally prove recovery only against a DeepCopy of the input object.
	encoded, err := json.Marshal(workflow)
	if err != nil {
		return nil, err
	}
	client.existing = &wfv1.Workflow{}
	if err := json.Unmarshal(encoded, client.existing); err != nil {
		return nil, err
	}
	client.existing.UID = "workflow-uid"
	client.existing.ResourceVersion = "17"
	client.existing.CreationTimestamp = metav1.Now()
	client.existing.Status.Phase = wfv1.WorkflowRunning
	client.existing.Labels["workflows.argoproj.io/phase"] = "Running"
	return client.existing.DeepCopy(), nil
}
func (client *persistedWorkflowClient) Delete(context.Context, string, metav1.DeleteOptions) error {
	return errors.New("unexpected Workflow deletion")
}
