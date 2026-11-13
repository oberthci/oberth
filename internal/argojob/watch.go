package argojob

import (
	"errors"
	"fmt"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

// ObservationHub supplies shared, read-only native objects. Reads that gate
// mutations continue to use the live clients.
type ObservationHub interface {
	Workflow(string) (*wfv1.Workflow, bool)
	PodsOf(string) []*corev1.Pod
	Pods() []*corev1.Pod
	Healthy() bool
	Synced() bool
	UnhealthySince() (time.Time, bool)
}

var errWatchUnavailable = errors.New("argojob: cluster observation unavailable")

// WithWatchHub connects read-only consumers to the namespace's shared cache.
func (controller *Controller) WithWatchHub(hub ObservationHub) *Controller {
	controller.hub = hub
	if controller.seeder != nil {
		controller.seeder.hub = hub
		controller.seeder.workflows = controller.workflows
	}
	return controller
}

func (controller *Controller) observationUnavailable() *model.ExecutionObservation {
	message := "Execution observation is unavailable."
	if controller.hub != nil && controller.hub.Synced() {
		if since, ok := controller.hub.UnhealthySince(); ok {
			message = fmt.Sprintf("status unknown: cluster API unreachable since %s", since.UTC().Format("15:04 UTC"))
		}
	}
	return &model.ExecutionObservation{State: "unavailable", Message: message}
}

func (controller *Controller) observedPodOwned(pod *corev1.Pod) bool {
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "Workflow" || owner.APIVersion != "argoproj.io/v1alpha1" || owner.Name != pod.Labels[argoWorkflowLabel] || owner.UID == "" {
		return false
	}
	workflow, found := controller.hub.Workflow(owner.Name)
	return found && workflow.UID == owner.UID
}
