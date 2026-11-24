package cronsync

import (
	"fmt"
	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	"maps"
	"slices"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

const scheduleParameter = "oberth-schedule"
const triggerParameter = "oberth-trigger"
const previousAdmissionAnnotation = "oberth.ci/previous-admission"
const controllerSelector = "workflows.argoproj.io/controller-instanceid"

func parameter(spec *wfv1.WorkflowSpec, name string) string {
	result := ""
	found := false
	for _, p := range spec.Arguments.Parameters {
		if p.Name != name {
			continue
		}
		if found || p.Value == nil || p.ValueFrom != nil || p.Default != nil {
			return ""
		}
		found = true
		result = p.Value.String()
	}
	return result
}
func owned(object *wfv1.CronWorkflow) (int64, string, bool) {
	entry := parameter(&object.Spec.WorkflowSpec, scheduleParameter)
	id, err := nativebinding.CronRepositoryID(object.Name, entry)
	return id, entry, err == nil && object.Labels[nativebinding.ManagedLabel] == nativebinding.ManagedValue
}

// render copies the sealed workflowSpec unchanged; only literal parent metadata is built.
func render(namespace, instanceID string, request Preparation, prepared *admissioncontract.PreparedAdmission) (*wfv1.CronWorkflow, error) {
	if prepared == nil {
		return nil, problem(badPrepared, nil)
	}
	view := prepared.View()
	if view.Workflow == nil || namespace == "" || !preparedMatches(view, request) {
		return nil, problem(badPrepared, nil)
	}
	// A pin is authorised against its producer at admission only, and a
	// parent outlives that check, so a schedule loads only its own source.
	if i := slices.IndexFunc(view.SourceBindings, func(b sourcecontract.Binding) bool { return b.Descriptor.Kind == sourcecontract.Pinned }); i >= 0 {
		refusal := problem(pinRefused, nil)
		refusal.Detail = fmt.Sprintf("entry %q: field %q has value pinned step %q: %s.", request.Entry.Name, "source", view.SourceBindings[i].Path, messages()[pinRefused].remedy)
		return nil, refusal
	}
	name, err := nativebinding.CronName(request.Repository.ID, request.Entry.Name)
	if err != nil {
		return nil, err
	}
	labels, annotations := maps.Clone(view.Workflow.Labels), maps.Clone(view.Workflow.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[nativebinding.AdmissionAnnotation] = view.Digest
	delete(labels, nativebinding.RunLabel)
	delete(labels, controllerSelector)
	delete(annotations, nativebinding.RunIDAnnotation)
	object := &wfv1.CronWorkflow{
		TypeMeta:   metav1.TypeMeta{APIVersion: wfv1.SchemeGroupVersion.String(), Kind: "CronWorkflow"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{nativebinding.ManagedLabel: nativebinding.ManagedValue}},
		Spec: wfv1.CronWorkflowSpec{
			Schedules:                  []string{request.Entry.CronExpression()},
			Timezone:                   request.Entry.Timezone,
			ConcurrencyPolicy:          wfv1.ConcurrencyPolicy(request.Entry.ConcurrencyPolicy),
			StartingDeadlineSeconds:    ptr.To(int64(86400)),
			SuccessfulJobsHistoryLimit: ptr.To(int32(2147483647)),
			FailedJobsHistoryLimit:     ptr.To(int32(2147483647)),
			WorkflowSpec:               view.Workflow.Spec,
			WorkflowMetadata:           &metav1.ObjectMeta{Labels: labels, Annotations: annotations, Finalizers: []string{nativebinding.RecordFinalizer}},
		},
	}
	instanceid.NewService(instanceID).Label(object)
	instanceid.NewService(instanceID).Label(object.Spec.WorkflowMetadata)
	return object, nil
}

func preparedMatches(view admissioncontract.PreparedView, request Preparation) bool {
	return view.Repository == request.Repository.Qualified && view.RunID == "" && view.Trigger == model.TriggerSchedule && view.ScheduleEntry == request.Entry.Name && view.Ref == request.Revision.Name && view.CommitSHA == request.Revision.CommitSHA && view.ObjectSHA == request.Revision.ObjectSHA
}
