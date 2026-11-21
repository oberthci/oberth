package app

import (
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

func audit(run stateport.Acceptance, reason string) model.AuditAction {
	v := run.Prepared.View()
	details := map[string]any{
		"reason": reason, "repo": v.Repository, "ref": v.Ref, "commit_sha": v.CommitSHA, "object_sha": v.ObjectSHA,
		"trigger": v.Trigger, "engine": "argo", "workflow": nativebinding.WorkflowName(v.RunID), "pipeline_file": v.SelectedFile,
		"namespace": v.Identity.Namespace, "service_account": v.Identity.Pipeline.Name,
		"automount_service_account_token": v.Workflow.Spec.AutomountServiceAccountToken, "granted_step_credentials": v.Grants,
		"sources": v.SourceBindings, "source_receipt": v.SourceReceipt, "spec_digest": v.SpecDigest,
	}
	raw, _ := json.Marshal(details)
	return model.AuditAction{Action: nativebinding.TierValue(run.Release) + ".argo.submit.binding", ResourceType: "run", ResourceID: v.RunID, Actor: run.Request.Actor.Identity, Details: string(raw)}
}
