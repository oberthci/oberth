package admissioncontract

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// PreparedAdmission keeps the sealed submission state private.
// C4 Prepare seals after native transformation, validation and planning;
// ScheduledChild binds that retained admission to its native child.
type PreparedAdmission struct {
	view PreparedView
}

// PreparedView is the value projection supplied by C4 and copied by Seal and View.
// Digest fields are computed by Seal; supplied values are ignored.
type PreparedView struct {
	Workflow       *wfv1.Workflow                         `json:"workflow"`
	SpecDigest     string                                 `json:"spec_digest"`
	Digest         string                                 `json:"digest"`
	Repository     string                                 `json:"repo"`
	RunID          string                                 `json:"run_id"`
	Ref            string                                 `json:"ref"`
	ObjectSHA      string                                 `json:"object_sha"`
	CommitSHA      string                                 `json:"commit_sha"`
	Trigger        model.RunTrigger                       `json:"trigger"`
	ScheduleEntry  string                                 `json:"schedule_entry"`
	SelectedFile   string                                 `json:"selected_file"`
	FileBlob       string                                 `json:"file_blob"`
	SourceReceipt  sourcecontract.Receipt                 `json:"source_receipt"`
	SourceBindings []sourcecontract.Binding               `json:"source_bindings"`
	Defaults       runtimeconfig.WorkflowDefaultsSnapshot `json:"defaults"`
	Templates      []TemplateSnapshot                     `json:"templates"`
	Identity       IdentityEvidence                       `json:"identity"`
	Grants         []GrantEvidence                        `json:"grants"`
	Plan           Plan                                   `json:"plan"`
	Findings       []Finding                              `json:"findings"`
}
