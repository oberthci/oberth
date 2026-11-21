package admissioncontract

import (
	"context"
	"encoding/json"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

type PrepareRequest struct {
	Mode            Mode                                   `json:"mode"`
	Repository      string                                 `json:"repo"`
	RunID           string                                 `json:"run_id"`
	Ref             string                                 `json:"ref"`
	ObjectSHA       string                                 `json:"object_sha"`
	CommitSHA       string                                 `json:"commit_sha"`
	Trigger         model.RunTrigger                       `json:"trigger"`
	ScheduleEntry   string                                 `json:"schedule_entry"`
	SelectedFile    string                                 `json:"selected_file"`
	FileBlob        string                                 `json:"file_blob"`
	Document        []byte                                 `json:"document"`
	Defaults        runtimeconfig.WorkflowDefaultsSnapshot `json:"defaults"`
	Templates       []TemplateSnapshot                     `json:"templates"`
	SourceReceipt   sourcecontract.Receipt                 `json:"source_receipt"`
	IdentityRequest IdentityRequest                        `json:"identity_request"`
	Grants          []GrantEvidence                        `json:"grants"`
}

// ScheduleParent identifies preparation of a reusable schedule without starting a run.
func ScheduleParent(mode Mode, trigger model.RunTrigger) bool {
	return mode == ModeInspect && trigger == model.TriggerSchedule
}

type PrepareHooks struct {
	// OfflineValidation defers installation-dependent checks for the local CLI.
	OfflineValidation bool
	// LoginAudience is supplied by native delivery when configured.
	LoginAudience string
	// ReleaseFacts reads server evidence after authored admission checks.
	ReleaseFacts func(context.Context) (ReleaseFacts, error)
	// BindSource resolves one effective source input after admission. C4 assigns
	// Template, Artifact and Path; the hook supplies the exact source evidence.
	BindSource       func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error)
	TransformNative  func(context.Context, *wfv1.Workflow, TransformInput) error
	VerifyIdentities func(context.Context, IdentityRequest) (IdentityEvidence, error)
}

type TransformInput struct {
	// TransformNative may set HoldArmed on existing entries after installing the wrapper.
	// All other plan facts remain owned by admission.
	Plan     Plan
	Mode     Mode
	Identity IdentityEvidence
	// CredentialSteps carries the granted step key for every credential template, including unused ones.
	CredentialSteps map[string]string
	// SecretGrants holds, per template, grants only for paths that template declares.
	SecretGrants   map[string][]GrantEvidence
	SourceBindings []sourcecontract.Binding
	Defaults       runtimeconfig.WorkflowDefaultsSnapshot
}

type IdentityRequest struct {
	Mode            Mode   `json:"mode"`
	Namespace       string `json:"namespace"`
	PipelineAccount string `json:"pipeline_account"`
	ResourceAccount string `json:"resource_account"`
}

type TemplateSnapshot struct {
	Namespace       string          `json:"namespace"`
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resource_version"`
	Spec            json.RawMessage `json:"spec"`
}

type AccountEvidence struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type IdentityEvidence struct {
	Namespace string           `json:"namespace"`
	Pipeline  AccountEvidence  `json:"pipeline"`
	Resource  *AccountEvidence `json:"resource,omitempty"`
	State     EvidenceState    `json:"state"` // verified|live_required|unknown
}

type GrantEvidence struct {
	Repository string        `json:"repo"`
	File       string        `json:"file"`
	Step       string        `json:"step"`
	Kind       string        `json:"kind"`
	Path       string        `json:"path"`
	Audience   string        `json:"audience"`
	Revision   string        `json:"revision"`
	State      EvidenceState `json:"state"` // verified|live_required|unknown
}

type Finding struct {
	// Installation marks evidence supplied by the installation, not a YAML fault.
	Installation bool   `json:"installation,omitempty"`
	Path         string `json:"path"`
	Code         string `json:"code"`
	Reason       string `json:"reason"`
	Remedy       string `json:"remedy"`
	// Command is the admission-produced fix, separate from the bounded remedy.
	Command string `json:"command,omitempty"`
}

// Key returns the shared identity/grant tuple while evidence keeps its flat wire fields.
func (g GrantEvidence) Key() identityname.Key {
	return identityname.Key{Repository: g.Repository, File: g.File, Step: g.Step}
}
