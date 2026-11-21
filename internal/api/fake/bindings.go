package fake

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func Bindings() apicontract.Bindings {
	var b apicontract.Bindings
	bindRun(&b)
	bindAction(&b)
	bindIdentity(&b)
	bindIssue(&b)
	bindObservation(&b)
	return b
}

func bindRun(b *apicontract.Bindings) {
	b.Logs.Handler = &Handler[apicontract.LogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{}}
	b.RunCheck.Handler = &Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{Findings: []apicontract.Finding{}, Checks: []apicontract.Check{}}}
	b.RunGet.Handler = &Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{}}
	b.RunList.Handler = &Handler[apicontract.RunListArgs, apicontract.RunPage]{Value: apicontract.RunPage{Runs: []stepmodel.RunSummary{}}}
	b.RunLogs.Handler = &Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{}}
	b.Status.Handler = &Handler[apicontract.StatusArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{}}
	b.Wait.Handler = &Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{}}
}

func bindAction(b *apicontract.Bindings) {
	b.Promote.Handler = &Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{}}
	b.PromoteRetry.Handler = &Handler[apicontract.PromoteRetryArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{}}
	b.PromoteStatus.Handler = &Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: apicontract.PromotionWaitResult{}}
	b.PromotionList.Handler = &Handler[apicontract.PromotionListArgs, apicontract.PromotionPage]{Value: apicontract.PromotionPage{Promotions: []model.Promotion{}}}
	b.PublicationAbandon.Handler = &Handler[apicontract.PublicationAbandonArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.PublishRetry.Handler = &Handler[apicontract.PublishRetryArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.ReceiveOutbox.Handler = &Handler[apicontract.ReceiveOutboxArgs, apicontract.OutboxResult]{Value: apicontract.OutboxResult{Records: []apicontract.OutboxRecord{}}}
	b.RunCancel.Handler = &Handler[apicontract.RunCancelArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.RunHoldAbandon.Handler = &Handler[apicontract.RunHoldAbandonArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.RunHoldExec.Handler = &Handler[apicontract.RunHoldExecArgs, apicontract.ExecResult]{Value: apicontract.ExecResult{}}
	b.RunHoldList.Handler = &Handler[apicontract.RunHoldListArgs, apicontract.HoldPage]{Value: apicontract.HoldPage{Holds: []model.LiveHold{}}}
	b.RunPurge.Handler = &Handler[apicontract.RunPurgeArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.RunRerun.Handler = &Handler[apicontract.RunRerunArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.RunWait.Handler = &Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{}}
	b.Sync.Handler = &Handler[apicontract.SyncArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.PublicationList.Handler = &Handler[apicontract.PublicationListArgs, apicontract.PublicationPage]{Value: apicontract.PublicationPage{Publications: []model.Publication{}}}
}

func bindIdentity(b *apicontract.Bindings) {
	b.SecretPut.Handler = &Handler[apicontract.SecretPutArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.AccessAllow.Handler = &Handler[apicontract.AccessAllowArgs, apicontract.Grant]{Value: apicontract.Grant{}}
	b.AccessDismiss.Handler = &Handler[apicontract.AccessDismissArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.AccessList.Handler = &Handler[apicontract.AccessListArgs, apicontract.GrantPage]{Value: apicontract.GrantPage{Grants: []apicontract.Grant{}}}
	b.AccessRevoke.Handler = &Handler[apicontract.AccessRevokeArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.AdminAccessList.Handler = &Handler[apicontract.AdminAccessListArgs, apicontract.GrantPage]{Value: apicontract.GrantPage{Grants: []apicontract.Grant{}}}
	b.AdminAuditRecord.Handler = &Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.AdminIdle.Handler = &Handler[apicontract.AdminIdleArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Parts: []apicontract.PartStatus{}}}
	b.AdminPublicIdentity.Handler = &Handler[apicontract.AdminPublicIdentityArgs, apicontract.PublicIdentity]{Value: apicontract.PublicIdentity{}}
	b.AdminReadiness.Handler = &Handler[apicontract.AdminReadinessArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Parts: []apicontract.PartStatus{}}}
	b.AdminVersion.Handler = &Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Parts: []apicontract.PartStatus{}}}
	b.ClientRenew.Handler = &Handler[apicontract.ClientRenewArgs, apicontract.Identity]{Value: apicontract.Identity{Scope: []string{}}}
	b.RepoAdd.Handler = &Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{}}
	b.RepoArchive.Handler = &Handler[apicontract.RepoArchiveArgs, apicontract.Repository]{Value: apicontract.Repository{}}
	b.RepoList.Handler = &Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{}}}
	b.RepoRemove.Handler = &Handler[apicontract.RepoRemoveArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.RepoUnarchive.Handler = &Handler[apicontract.RepoUnarchiveArgs, apicontract.Repository]{Value: apicontract.Repository{}}
	b.RepoVerify.Handler = &Handler[apicontract.RepoVerifyArgs, apicontract.Repository]{Value: apicontract.Repository{}}
	b.UplinkAdd.Handler = &Handler[apicontract.UplinkAddArgs, apicontract.Identity]{Value: apicontract.Identity{Scope: []string{}}}
	b.UplinkCert.Handler = &Handler[apicontract.UplinkCertArgs, apicontract.Identity]{Value: apicontract.Identity{Scope: []string{}}}
	b.UplinkInvite.Handler = &Handler[apicontract.UplinkInviteArgs, model.Invitation]{Value: model.Invitation{Scope: []string{}}}
	b.UplinkList.Handler = &Handler[apicontract.UplinkListArgs, apicontract.IdentityPage]{Value: apicontract.IdentityPage{Uplinks: []apicontract.Uplink{}}}
	b.UplinkRevoke.Handler = &Handler[apicontract.UplinkRevokeArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.UplinkRotate.Handler = &Handler[apicontract.UplinkRotateArgs, apicontract.Identity]{Value: apicontract.Identity{Scope: []string{}}}
	b.UplinkScope.Handler = &Handler[apicontract.UplinkScopeArgs, apicontract.Identity]{Value: apicontract.Identity{Scope: []string{}}}
	b.UpstreamAdd.Handler = &Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]{Value: apicontract.Upstream{}}
	b.UpstreamList.Handler = &Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{}}}
	b.UpstreamProvideKey.Handler = &Handler[apicontract.UpstreamProvideKeyArgs, apicontract.Upstream]{Value: apicontract.Upstream{}}
	b.UpstreamRemove.Handler = &Handler[apicontract.UpstreamRemoveArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.WorkflowCapabilityGet.Handler = &Handler[apicontract.WorkflowCapabilityGetArgs, apicontract.Capability]{Value: apicontract.Capability{}}
	b.WorkflowCapabilityGrant.Handler = &Handler[apicontract.WorkflowCapabilityGrantArgs, apicontract.Capability]{Value: apicontract.Capability{}}
	b.WorkflowCapabilityRevoke.Handler = &Handler[apicontract.WorkflowCapabilityRevokeArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
}

func bindIssue(b *apicontract.Bindings) {
	b.IssueImport.Handler = &Handler[apicontract.IssueImportArgs, stateport.IssueImportResult]{Value: stateport.IssueImportResult{}}
	b.IssueAppend.Handler = &Handler[apicontract.IssueAppendArgs, apicontract.IssueResult]{Value: apicontract.IssueResult{}}
	b.IssueClose.Handler = &Handler[apicontract.IssueCloseArgs, apicontract.IssueResult]{Value: apicontract.IssueResult{}}
	b.IssueCreate.Handler = &Handler[apicontract.IssueCreateArgs, apicontract.IssueResult]{Value: apicontract.IssueResult{}}
	b.IssueDelete.Handler = &Handler[apicontract.IssueDeleteArgs, apicontract.IssueResult]{Value: apicontract.IssueResult{}}
	b.IssueGet.Handler = &Handler[apicontract.IssueGetArgs, model.Issue]{Value: model.Issue{}}
	b.IssueGetMany.Handler = &Handler[apicontract.IssueGetManyArgs, apicontract.IssueManyResult]{Value: apicontract.IssueManyResult{Issues: []apicontract.IssueResult{}}}
	b.IssueList.Handler = &Handler[apicontract.IssueListArgs, apicontract.IssuePage]{Value: apicontract.IssuePage{Issues: []model.Issue{}}}
	b.IssueLock.Handler = &Handler[apicontract.IssueLockArgs, model.IssueLock]{Value: model.IssueLock{}}
	b.IssueReopen.Handler = &Handler[apicontract.IssueReopenArgs, apicontract.IssueResult]{Value: apicontract.IssueResult{}}
	b.IssueUpdate.Handler = &Handler[apicontract.IssueUpdateArgs, apicontract.IssueResult]{Value: apicontract.IssueResult{}}
}

func bindObservation(b *apicontract.Bindings) {
	b.AuditList.Handler = &Handler[apicontract.AuditListArgs, apicontract.AuditPage]{Value: apicontract.AuditPage{Records: []apicontract.AuditRecord{}}}
	b.EventsWait.Handler = &Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Value: apicontract.EventResult{EventPage: model.EventPage{Events: []model.AgentEvent{}}}}
	b.ScheduleList.Handler = &Handler[apicontract.ScheduleListArgs, apicontract.SchedulePage]{Value: apicontract.SchedulePage{Schedules: []apicontract.Schedule{}}}
	b.SchedulePause.Handler = &Handler[apicontract.SchedulePauseArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.ScheduleResume.Handler = &Handler[apicontract.ScheduleResumeArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.ScheduleTrigger.Handler = &Handler[apicontract.ScheduleTriggerArgs, apicontract.Receipt]{Value: apicontract.Receipt{}}
	b.SyncStatus.Handler = &Handler[apicontract.SyncStatusArgs, apicontract.SyncStatus]{Value: apicontract.SyncStatus{Locks: []apicontract.LockState{}}}
	b.SystemStatus.Handler = &Handler[apicontract.SystemStatusArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Parts: []apicontract.PartStatus{}}}
}
