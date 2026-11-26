package service

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
)

// Bindings connects every canonical tool declaration to its service handler.
// Composition passes this table directly to the transport registry.
func (s *Service) Bindings() apicontract.Bindings {
	return apicontract.Bindings{
		RunsTools:      s.bindRunsTools(),
		IssuesTools:    s.bindIssuesTools(),
		AccessTools:    s.bindAccessTools(),
		PromotionTools: s.bindPromotionTools(),
		AdminTools:     s.bindAdminTools(),
		EventsTools:    s.bindEventsTools(),
	}
}

func (s *Service) bindRunsTools() apicontract.RunsTools {
	return apicontract.RunsTools{
		Logs:           bindTool(s.PrepareLogs),
		RunCancel:      bindTool(s.PrepareRunCancel),
		RunCheck:       bindTool(s.PrepareRunCheck),
		RunGet:         bindTool(s.PrepareRunGet),
		RunHoldAbandon: bindTool(s.PrepareRunHoldAbandon),
		RunHoldExec:    bindTool(s.PrepareRunHoldExec),
		RunHoldList:    bindTool(s.PrepareRunHoldList),
		RunList:        bindTool(s.PrepareRunList),
		RunLogs:        bindTool(s.PrepareRunLogs),
		RunPurge:       bindTool(s.PrepareRunPurge),
		RunResume:      bindTool(s.PrepareRunResume),
		RunRerun:       bindTool(s.PrepareRunRerun),
		RunWait:        bindTool(s.PrepareRunWait),
		Status:         bindTool(s.PrepareStatus),
		Wait:           bindTool(s.PrepareWait),
	}
}

func (s *Service) bindIssuesTools() apicontract.IssuesTools {
	return apicontract.IssuesTools{
		IssueImport:  bindTool(s.PrepareIssueImport),
		IssueAppend:  bindTool(s.PrepareIssueAppend),
		IssueClose:   bindTool(s.PrepareIssueClose),
		IssueCreate:  bindTool(s.PrepareIssueCreate),
		IssueDelete:  bindTool(s.PrepareIssueDelete),
		IssueGet:     bindTool(s.PrepareIssueGet),
		IssueGetMany: bindTool(s.PrepareIssueGetMany),
		IssueList:    bindTool(s.PrepareIssueList),
		IssueLock:    bindTool(s.PrepareIssueLock),
		IssueReopen:  bindTool(s.PrepareIssueReopen),
		IssueUpdate:  bindTool(s.PrepareIssueUpdate),
	}
}

func (s *Service) bindAccessTools() apicontract.AccessTools {
	return apicontract.AccessTools{
		SecretPut:                bindTool(s.PrepareSecretPut),
		AccessAllow:              bindTool(s.PrepareAccessAllow),
		AccessDismiss:            bindTool(s.PrepareAccessDismiss),
		AccessList:               bindTool(s.PrepareAccessList),
		AccessRevoke:             bindTool(s.PrepareAccessRevoke),
		WorkflowCapabilityGet:    bindTool(s.PrepareWorkflowCapabilityGet),
		WorkflowCapabilityGrant:  bindTool(s.PrepareWorkflowCapabilityGrant),
		WorkflowCapabilityRevoke: bindTool(s.PrepareWorkflowCapabilityRevoke),
	}
}

func (s *Service) bindPromotionTools() apicontract.PromotionTools {
	return apicontract.PromotionTools{
		Promote:            bindTool(s.PreparePromote),
		PromoteRetry:       bindTool(s.PreparePromoteRetry),
		PromoteStatus:      bindTool(s.PreparePromoteStatus),
		PromotionList:      bindTool(s.PreparePromotionList),
		PublicationAbandon: bindTool(s.PreparePublicationAbandon),
		PublishRetry:       bindTool(s.PreparePublishRetry),
		ReceiveOutbox:      bindTool(s.PrepareReceiveOutbox),
		Sync:               bindTool(s.PrepareSync),
		PublicationList:    bindTool(s.PreparePublicationList),
	}
}

func (s *Service) bindAdminTools() apicontract.AdminTools {
	return apicontract.AdminTools{
		AdminAccessList:     bindTool(s.PrepareAdminAccessList),
		AdminAuditRecord:    bindTool(s.PrepareAdminAuditRecord),
		AdminIdle:           bindTool(s.PrepareAdminIdle),
		AdminPublicIdentity: bindTool(s.PrepareAdminPublicIdentity),
		AdminReadiness:      bindTool(s.PrepareAdminReadiness),
		AdminVersion:        bindTool(s.PrepareAdminVersion),
		AuditList:           bindTool(s.PrepareAuditList),
		ClientRenew:         bindTool(s.PrepareClientRenew),
		RepoAdd:             bindTool(s.PrepareRepoAdd),
		RepoArchive:         bindTool(s.PrepareRepoArchive),
		RepoList:            bindTool(s.PrepareRepoList),
		RepoRemove:          bindTool(s.PrepareRepoRemove),
		RepoUnarchive:       bindTool(s.PrepareRepoUnarchive),
		RepoVerify:          bindTool(s.PrepareRepoVerify),
		UplinkAdd:           bindTool(s.PrepareUplinkAdd),
		UplinkCert:          bindTool(s.PrepareUplinkCert),
		UplinkInvite:        bindTool(s.PrepareUplinkInvite),
		UplinkList:          bindTool(s.PrepareUplinkList),
		UplinkRevoke:        bindTool(s.PrepareUplinkRevoke),
		UplinkRotate:        bindTool(s.PrepareUplinkRotate),
		UplinkScope:         bindTool(s.PrepareUplinkScope),
		UpstreamAdd:         bindTool(s.PrepareUpstreamAdd),
		UpstreamList:        bindTool(s.PrepareUpstreamList),
		UpstreamProvideKey:  bindTool(s.PrepareUpstreamProvideKey),
		UpstreamRemove:      bindTool(s.PrepareUpstreamRemove),
	}
}

func (s *Service) bindEventsTools() apicontract.EventsTools {
	return apicontract.EventsTools{
		EventsWait:      bindTool(s.PrepareEventsWait),
		ScheduleList:    bindTool(s.PrepareScheduleList),
		SchedulePause:   bindTool(s.PrepareSchedulePause),
		ScheduleResume:  bindTool(s.PrepareScheduleResume),
		ScheduleTrigger: bindTool(s.PrepareScheduleTrigger),
		SyncStatus:      bindTool(s.PrepareSyncStatus),
		SystemStatus:    bindTool(s.PrepareSystemStatus),
	}
}

func bindTool[I, O any](prepare func(context.Context, apicontract.Caller, I) (apicontract.Call[O], error)) apicontract.Binding[I, O] {
	return apicontract.Binding[I, O]{Handler: apicontract.HandlerFunc[I, O](prepare)}
}
