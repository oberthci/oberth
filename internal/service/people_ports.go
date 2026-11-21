package service

import (
	"context"
	"crypto/x509"
	"io"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type IssueStore interface {
	ImportIssues(context.Context, stateport.Action, stateport.IssueImport) (stateport.IssueImportResult, error)
	ScopedIssue(context.Context, stateport.IssueScope, int64) (model.Issue, error)
	ScopedIssues(context.Context, stateport.IssueScope, model.IssueFilter) (stateport.IssuePage, error)
	CreateIssue(context.Context, stateport.Action, model.Issue) (model.Issue, error)
	UpdateIssue(context.Context, stateport.Action, model.IssuePatch) (model.Issue, error)
	ChangeIssueLock(context.Context, stateport.Action, stateport.IssueLockRequest) error
	CloseIssue(context.Context, stateport.Action, stateport.IssueClose) error
	ReopenIssue(context.Context, stateport.Action, stateport.IssueClose) error
	DeleteIssue(context.Context, stateport.Action, stateport.IssueClose) error
}
type EnrollmentStore interface {
	InstallAdmin(context.Context, stateport.Action, stateport.IdentityRegistration) error
	Invite(context.Context, stateport.Action, stateport.Invitation) error
	ReserveInvitation(context.Context, stateport.InvitationReservation) (model.Invitation, error)
	Enroll(context.Context, stateport.Enrollment) error
	RegisterIdentity(context.Context, stateport.Action, stateport.IdentityRegistration) error
	AddCertificatePin(context.Context, stateport.Action, stateport.CertificateBinding) error
	Key(context.Context, string) (stateport.Key, error)
	IdentityKeys(context.Context, string) ([]stateport.Key, error)
	SetKeyScope(context.Context, stateport.Action, string, []string) error
	RevokeKey(context.Context, stateport.Action, string) error
	ReplaceKey(context.Context, stateport.Action, stateport.KeyReplacement) error
}
type Issuer interface {
	Issue(string, []byte, time.Duration) ([]byte, *x509.Certificate, error)
}

// AuthorityPublication owns C10's wide barrier. Apply must run exactly once under
// that barrier and publish the returned effective snapshot before releasing it.
type AuthorityPublication interface {
	Publish(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error
}
type GrantStore interface {
	ChangeGrant(context.Context, stateport.Action, stateport.GrantChange) (stateport.AuthoritySnapshot, error)
	Grants(context.Context, []string) (stateport.AuthoritySnapshot, error)
}
type ClusterPolicy interface {
	// ValidateCapability applies admission's canonical shape/account/action rules.
	ValidateCapability(context.Context, apicontract.Capability) error
	ServerNamespace(string) bool
	Namespaced(context.Context, string, string) (bool, error)
}

// Administration adapts installation-owned key storage/probing and runtime health.
// PrepareUpstream scans host trust; its Persist and PersistKey run after the audit commit.
type PreparedUpstream struct {
	Binding           stateport.Upstream
	Offered, Replaced []string
	Persist           func(context.Context) error
}

type Administration interface {
	ReceiveOutbox(context.Context, stateport.Action, []string, apicontract.ReceiveOutboxArgs) (apicontract.OutboxResult, error)
	PrepareUpstream(context.Context, apicontract.UpstreamAddArgs) (PreparedUpstream, error)
	PersistKey(context.Context, apicontract.UpstreamProvideKeyArgs) (apicontract.Upstream, error)
	ProbeRepository(context.Context, stateport.Repository) error
	PublicIdentity(context.Context, string) (apicontract.PublicIdentity, error)
	ProbeUpstream(context.Context, string, string) (apicontract.Upstream, error)
	Status(context.Context, string) (apicontract.SystemStatus, error)
}
type AdminStore interface {
	RegisterRepository(context.Context, stateport.Action, string) (stateport.Repository, error)
	ArchiveRepository(context.Context, stateport.Action, int64) error
	UnarchiveRepository(context.Context, stateport.Action, int64) error
	CheckRepositoryRemoval(context.Context, int64) error
	RemoveRepository(context.Context, stateport.Action, int64) error
	RegisterUpstreamBinding(context.Context, stateport.Action, stateport.Upstream) error
	UpstreamBindings(context.Context) ([]stateport.Upstream, error)
	RemoveUpstream(context.Context, stateport.Action, string) error
	RecordAdministration(context.Context, stateport.Action, model.AuditAction) error
	AdministrativeAudit(context.Context, []string, stateport.AdministrativeFilter) ([]stateport.AuditRecord, error)
}
type Snapshotter interface {
	Snapshot(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error)
}

// StateMetadata supplies live database identity for administrator status.
type StateMetadata interface {
	StateMetadata(context.Context) (int, stateport.Protocol, error)
}

// SecretWriter writes and deletes assigned step paths, without a read capability.
type SecretWriter interface {
	Put(context.Context, string, string, []byte) error
	Delete(context.Context, string, string) error
}

// People groups required people/authority dependencies; none can silently fall
// back to an unaudited implementation. Backup options bind the current store port.
// IssueWindows are the installation's stale windows for reads that name none.
type People struct {
	IssueWindows   model.IssueWindows
	StateMetadata  StateMetadata
	SecretWriter   SecretWriter
	ReleaseRoutes  workflowpolicy.ReleaseRoutes
	Schedules      Schedules
	Issues         IssueStore
	Enrollment     EnrollmentStore
	Grants         GrantStore
	Publication    AuthorityPublication
	Cluster        ClusterPolicy
	Administration Administration
	Admin          AdminStore
	Issuer         Issuer
	Backup         Snapshotter
	BackupOptions  stateport.Options
	CAPin          string
	PublicURL      string
	SSHURL         string
	Random         io.Reader
}
