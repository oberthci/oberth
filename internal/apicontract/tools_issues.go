package apicontract

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// IssuesTools declares the issues service operations.
type IssuesTools struct {
	IssueImport  Binding[IssueImportArgs, stateport.IssueImportResult] `tool:"issue_import" command:"issue import" flow:"true" access:"unscoped_admin" summary:"Import a complete issue file without overwriting records." desc:"Import a neutral JSON Lines file atomically. Validate with a dry run first; identical record-set replay reports already imported." mutation:"true" check:"issue_list"`
	IssueAppend  Binding[IssueAppendArgs, IssueResult]                 `tool:"issue_append" summary:"Append investigation notes without replacing the issue body." desc:"Append investigation notes without replacing the issue body. Read issue_get to verify the new revision." mutation:"true" check:"issue_get"`
	IssueClose   Binding[IssueCloseArgs, IssueResult]                  `tool:"issue_close" summary:"Close an issue after recording its resolution." desc:"Mark the selected issue closed after recording its resolution. Read issue_get to confirm the resulting state." mutation:"true" check:"issue_get"`
	IssueCreate  Binding[IssueCreateArgs, IssueResult]                 `tool:"issue_create" summary:"Create an issue with a title and body." desc:"Create an investigation issue with a title and complete body. Keep its ID and use issue_get for subsequent reads." mutation:"true" check:"issue_get"`
	IssueDelete  Binding[IssueDeleteArgs, IssueResult]                 `tool:"issue_delete" summary:"Delete an issue when retaining it is no longer appropriate." desc:"Delete an issue when retaining it is no longer appropriate. Refresh issue_list before choosing another record." mutation:"true" check:"issue_get"`
	IssueGet     Binding[IssueGetArgs, model.Issue]                    `tool:"issue_get" summary:"Read an issue." desc:"Read an authorized issue without acquiring or extending its lock. Use issue_lock only when coordinating an explicit edit." get:"/api/issues/{id}"`
	IssueGetMany Binding[IssueGetManyArgs, IssueManyResult]            `tool:"issue_get_many" summary:"Read complete authorized issues in request order." desc:"Read complete authorized issues in request order. Use issue_get for any row whose service-owned response budget refuses a full record."`
	IssueList    Binding[IssueListArgs, IssuePage]                     `tool:"issue_list" summary:"List authorized issue metadata before selecting full bodies." desc:"List authorized issue metadata and counts before selecting full bodies. Continue to older or newer pages with the returned cursors using the same filters." get:"/api/issues"`
	IssueLock    Binding[IssueLockArgs, model.IssueLock]               `tool:"issue_lock" summary:"Lock an issue before a coordinated edit." desc:"Acquire a coordination lock before editing an issue with another investigator. Use issue_update with the current revision after the lock succeeds." mutation:"true" check:"issue_get"`
	IssueReopen  Binding[IssueReopenArgs, IssueResult]                 `tool:"issue_reopen" summary:"Reopen an issue for further investigation." desc:"Reopen a previously closed issue when investigation must continue. Use issue_get to inspect its latest revision." mutation:"true" check:"issue_get"`
	IssueUpdate  Binding[IssueUpdateArgs, IssueResult]                 `tool:"issue_update" summary:"Update an issue at its expected revision." desc:"Replace the supplied issue fields while checking the expected revision. Read issue_get after a conflict before submitting another edit." mutation:"true" check:"issue_get"`
}
