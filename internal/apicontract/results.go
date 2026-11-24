package apicontract

import (
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// RunRecord is one run as run_get and run_list return it: the run projection
// and the recorded cause of the run, both visible only inside the caller's scope.
type RunRecord struct {
	stepmodel.RunSummary
	model.RunCause
	// SelectedFile is the explicit workflow choice, including refused admissions.
	SelectedFile string `json:"selected_file,omitempty"`
}

type RunPage struct {
	Runs       []RunRecord `json:"runs"`
	NextCursor string      `json:"next_cursor,omitempty"`
	// ServerTime is the server's clock when it read the page, so a reader ages the runs
	// against the clock that stamped them.
	ServerTime time.Time `json:"server_time,omitzero"`
}
type RunCounts struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}
type WaitResult struct {
	Run      *stepmodel.RunSummary `json:"run"`
	TimedOut bool                  `json:"timed_out"`
}
type LogResult struct {
	Containers    []Container      `json:"containers,omitzero"`
	ID            string           `json:"id,omitempty"`
	NodeID        string           `json:"node_id,omitempty"`
	Container     string           `json:"container,omitempty"`
	Content       string           `json:"content"`
	Meta          logcontract.Meta `json:"meta"`
	DisplayReason string           `json:"display_reason,omitempty"`
	Detail        string           `json:"detail,omitempty"`
}
type Container struct {
	NodeID    string `json:"node_id"`
	PodName   string `json:"pod_name"`
	Container string `json:"container"`
}
type ContainerPage struct {
	Containers []Container `json:"containers"`
}

// PromoteResult identifies the source evidence and the tested result of one promotion.
type PromoteResult struct {
	ID          string `json:"id"`
	TestedRunID string `json:"tested_run_id"`
	CommitSHA   string `json:"commit_sha"`
	RunID       string `json:"run_id"`
	State       string `json:"state"`
}

type Receipt struct {
	ID              string `json:"id"`
	State           string `json:"state,omitempty"`
	RestartRequired bool   `json:"restart_required,omitempty"`
	Warning         string `json:"warning,omitempty"`
}
type HoldPage struct {
	Holds []model.LiveHold `json:"holds"`
}
type ExecResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
}
type Finding struct {
	Command string `json:"command,omitempty"`
	Path    string `json:"path"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
	Remedy  string `json:"remedy"`
	Detail  string `json:"detail,omitempty"`
}
type Check struct {
	Name  string `json:"name"`
	State string `json:"state"`
}
type RunCheckResult struct {
	// ReleaseTag is set only after checking this proposed tag's release route and
	// live grant authority at CommitSHA; ordinary inspection leaves it empty.
	ReleaseTag   string                  `json:"release_tag,omitempty"`
	OK           bool                    `json:"ok"`
	Repo         string                  `json:"repo"`
	Ref          string                  `json:"ref"`
	ObjectSHA    *string                 `json:"object_sha"`
	CommitSHA    *string                 `json:"commit_sha"`
	SelectedFile *string                 `json:"selected_file"`
	SpecDigest   *string                 `json:"spec_digest"`
	Plan         *admissioncontract.Plan `json:"plan"`
	Findings     []Finding               `json:"findings"`
	Checks       []Check                 `json:"checks"`
}
type PromotionPage struct {
	Promotions []model.Promotion `json:"promotions"`
	NextCursor string            `json:"next_cursor,omitempty"`
}
type Grant struct {
	ID     string `json:"id"`
	Repo   string `json:"repo"`
	File   string `json:"file"`
	Step   string `json:"step"`
	Path   string `json:"path"`
	Status string `json:"status"`
	// Warning names follow-up work a saved grant change did not finish.
	Warning string `json:"warning,omitempty"`
}
type GrantPage struct {
	Grants []Grant `json:"grants"`
}

// IssuePage is one page of issue metadata, newest first. Counts covers every
// visible issue matching the repository, kind and query; Total the issues
// matching the whole filter, of which Offset precede this page. NextBefore
// continues to older issues and NextAfter to newer ones, each only when
// there are some.
type IssuePage struct {
	Issues     []model.Issue     `json:"issues"`
	Counts     model.IssueCounts `json:"counts"`
	Total      int               `json:"total"`
	Offset     int               `json:"offset"`
	NextBefore int64             `json:"next_before,omitempty"`
	NextAfter  int64             `json:"next_after,omitempty"`
}
type IssueResult struct {
	State         model.IssueState `json:"state,omitempty"`
	UpdatedAt     *time.Time       `json:"updated_at,omitempty"`
	Occurrences   int64            `json:"occurrences,omitempty"`
	AppendedBytes int              `json:"appended_bytes,omitempty"`
	ID            int64            `json:"id"`
	Issue         *model.Issue     `json:"issue,omitempty"`
	Error         *publicerr.Error `json:"error,omitempty"`
	Deleted       bool             `json:"deleted,omitempty"`
}
type IssueManyResult struct {
	Issues []IssueResult `json:"issues"`
}
type Repository struct {
	Repo          string                `json:"repo"`
	DefaultBranch string                `json:"default_branch"`
	Archived      bool                  `json:"archived"`
	BranchCount   *int                  `json:"branch_count"`
	LatestRun     *stepmodel.RunSummary `json:"latest_run"`
	RecentRuns    []stateport.RunMark   `json:"recent_runs,omitzero"`
}
type RepoPage struct {
	Upstreams    []Upstream   `json:"upstreams,omitempty"`
	Repositories []Repository `json:"repositories"`
}
type Identity struct {
	Identity    string   `json:"identity"`
	Fingerprint string   `json:"fingerprint"`
	Admin       bool     `json:"admin"`
	Scope       []string `json:"scope"`
	PublicKey   string   `json:"public_key,omitempty"`
	Certificate string   `json:"certificate,omitempty"`
}
type IdentityPage struct {
	Uplinks []Uplink `json:"uplinks"`
}
type Upstream struct {
	DisplayReason string   `json:"display_reason,omitempty"`
	Offered       []string `json:"offered,omitempty"`
	Replaced      []string `json:"replaced,omitempty"`
	Name          string   `json:"name"`
	BaseURL       string   `json:"base_url"`
	Org           string   `json:"org"`
	PublicKey     string   `json:"public_key,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	Pending       bool     `json:"pending"`
}
type UpstreamPage struct {
	Upstreams []Upstream `json:"upstreams"`
}
type AuditRecord struct {
	model.AuditAction
	SHA256         string `json:"sha256"`
	PreviousSHA256 string `json:"previous_sha256"`
}
type AuditPage struct {
	Records    []AuditRecord `json:"records"`
	NextCursor string        `json:"next_cursor,omitempty"`
}
type Schedule struct {
	Repo          string `json:"repo"`
	Entry         string `json:"entry"`
	Paused        bool   `json:"paused"`
	DisplayReason string `json:"display_reason"`
	Detail        string `json:"detail,omitempty"`
}
type SchedulePage struct {
	Schedules []Schedule `json:"schedules"`
}
type PartStatus struct {
	Remedy        string `json:"remedy,omitempty"`
	Name          string `json:"name"`
	Ready         bool   `json:"ready"`
	DisplayReason string `json:"display_reason"`
}

type SystemStatus struct {
	SchemaVersion int                `json:"schema_version,omitempty"`
	Protocol      stateport.Protocol `json:"protocol,omitzero"`
	Parts         []PartStatus       `json:"parts"`
	Ready         bool               `json:"ready"`
	Version       string             `json:"version"`
	DisplayReason string             `json:"display_reason"`
	Detail        string             `json:"detail,omitempty"`
}
type EventResult struct {
	model.EventPage
	TimedOut bool `json:"timed_out"`
}
type Uplink struct {
	Identity    string     `json:"identity"`
	Fingerprint string     `json:"fingerprint"`
	Admin       bool       `json:"admin"`
	Scope       []string   `json:"scope"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeen    *time.Time `json:"last_seen"`
	RevokedAt   *time.Time `json:"revoked_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	NoExpiry    bool       `json:"no_expiry"`
	Pending     bool       `json:"pending"`
}
type PublicIdentity struct {
	Name        string `json:"name"`
	PublicKey   string `json:"public_key,omitempty"`
	Certificate string `json:"certificate,omitempty"`
}

// PromotionWaitResult separates observation timeout from durable promotion state.
// A passed promotion carries the publication of its target branch.
type PromotionWaitResult struct {
	Promotion   *model.Promotion   `json:"promotion,omitempty"`
	Publication *model.Publication `json:"publication,omitempty"`
	TimedOut    bool               `json:"timed_out"`
}
type PublicationPage struct {
	Publications []model.Publication `json:"publications"`
	NextCursor   string              `json:"next_cursor,omitempty"`
}

func (v *WaitResult) waitTimedOut() { v.TimedOut = true }
func (v *EventResult) waitTimedOut() {
	v.TimedOut = true
	if v.Events == nil {
		v.Events = []model.AgentEvent{}
	}
}
func (v *PromotionWaitResult) waitTimedOut() { v.TimedOut = true }

type Capability struct {
	Namespace      string         `json:"namespace" required:"true"`
	ServiceAccount string         `json:"service_account" required:"true"`
	ResourceKinds  []ResourceKind `json:"resource_kinds,omitempty"`
	// Warning names follow-up work a saved grant did not finish; never stored.
	Warning string `json:"warning,omitempty"`
}
type ResourceKind struct {
	APIVersion string   `json:"api_version" required:"true"`
	Kind       string   `json:"kind" required:"true"`
	Actions    []string `json:"actions" required:"true"`
}
