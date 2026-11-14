package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const promotionCompensationTimeout = 30 * time.Second

// revokePolicySyncAdvisory is the warning returned by access_revoke to inform
// the caller that the Vault credentialed policy still has the exact-path grant.
// The server's own identity has no Vault policy-write capability, so the resync
// must be triggered externally.
const revokePolicySyncAdvisory = "Revocation is effective for new Oberth admissions immediately. " +
	"The Vault credentialed policy still has this path until re-synced: " +
	"run `oberth secretstore sync` (or `oberth install --install-secretstore --upgrade`) to remove it from the Vault policy."

// grantPolicySyncAdvisory is the warning returned by access_allow to inform
// the caller that the new grant is recorded but not yet live. The server
// refreshes its identity map on every successful reconciliation (issue #465),
// so the identity appears without a restart — but the Vault policy and the
// chart-managed ServiceAccount remain operator-gated: the server's own
// identity has no Vault policy-write capability (issue #427), and a plain
// `install --upgrade` without --install-secretstore skips the secret-store
// leg entirely (no identity produce, no policy sync, no ServiceAccount).
// Until both complete, admission fails closed: the grant exists in sqlite
// but the Vault layer refuses the path.
const grantPolicySyncAdvisory = "Grant recorded; the identity map refreshes automatically on the next reconcile — no restart needed. " +
	"The Vault policy does not include this path yet: run `oberth secretstore sync` under your own BAO_TOKEN, " +
	"then `oberth install --install-secretstore --upgrade` if this repo's ServiceAccount does not exist yet. " +
	"Until then, access fails closed."

type APIConfig struct {
	Administration Administration

	Tags                   *PushIngestor
	IssueStaleAfter        time.Duration
	IssueActiveStaleAfter  time.Duration
	WorkflowCapabilities   WorkflowCapabilityStore
	ReleasePreflight       ReleasePreflighter
	ClientCertificates     *ClientCertIssuer
	UplinkLifecycle        UplinkLifecycle
	DeclarationDismissal   DeclarationDismissal
	IssueLifecycle         IssueLifecycle
	SchedulingObserver     SchedulingObserver
	Runs                   RunResolver
	History                RunHistory
	Repositories           RepositoryReader
	Issues                 IssueRepository
	Promotions             PromotionRepository
	PromotionRuns          PromotionRunStore
	Enqueues               EnqueueObserver
	Git                    DeliveryGit
	Refs                   RefResolver
	Logs                   LogStore
	Artifacts              ArtifactStore
	Auditor                Auditor
	Health                 Health
	Signals                *Signals
	MaximumWait            time.Duration
	MutationGate           func(context.Context) error
	PromotionWorkspaceRoot string
	RemoveWorkspace        func(string) error
	SecretAccess           SecretAccessStore

	ReleaseWIFOverrides       ReleaseWIFOverrideStore
	ReleaseWIF                ReleaseWIFPolicy
	SecretStoreVerifier       SecretStoreVerifier
	SecretAccessReconciler    *AccessReconciler
	RepositoryArchiver        RepositoryArchiver
	ChangeRepositoryArchive   func(context.Context, string, bool, func() error) error
	RepositoryRemover         RepositoryRemover
	RemoveGitCache            func(string) error
	PromotionGrants           PromotionGrantStore
	PromotionGrantMaxLifetime time.Duration // Default 24h, hard ceiling 168h (7 days).
	// RunCancels backs run_cancel; nil disables the tool.
	RunCancels RunCancelStore
	// Uplinks re-resolves an uplink's current authority for the server's
	// own promotion retry (#1097); nil disables that retry.
	Uplinks UplinkDirectory
	// RunReruns backs run_rerun; nil disables the tool.
	RunReruns RunRerunStore
	// AssistHolds and RunAssists back run_hold_list, run_hold_exec,
	// run_hold_patch, run_continue and run_hold_abandon; nil disables them.
	AssistHolds AssistHoldOperator
	RunAssists  RunAssistStore
	Retention   *Retention
	// Schedules backs the schedule_* tools (#1106); nil disables them.
	Schedules ScheduleControl
	// BranchPrune and BranchDeletions back branch_delete_many,
	// branch_deletion_list (#1490) and branch_deletion_resolve (#1500); nil
	// disables them. ref_delete_many (#1504) also needs BranchPrune to
	// implement StrayRefGit.
	BranchPrune     BranchPruneGit
	BranchDeletions BranchDeletionRecords
	// DeletionQueued wakes the upstream deletion propagator; optional.
	DeletionQueued func()
	// AgentEvents backs events_subscribe, events_since, sync_status, the MCP
	// push stream and promote prune_source (#1514); nil disables them.
	AgentEvents      AgentEventStore
	AgentEventPolicy AgentEventPolicy
}

type API struct {
	administration  Administration
	adminBackupLock *sync.Mutex

	tags                   *PushIngestor
	branchPrune            BranchPruneGit
	branchDeletions        BranchDeletionRecords
	deletionQueued         func()
	receiveOutbox          ReceiveOutbox
	receiveHandler         gitcache.ReceiveHandler
	branchCounts           *branchCountCache
	issueStaleAfter        time.Duration
	issueActiveStaleAfter  time.Duration
	issueObservedAt        time.Time
	workflowCapabilities   WorkflowCapabilityStore
	promotionProgress      *sync.Map
	releasePreflight       ReleasePreflighter
	clientCertificates     *ClientCertIssuer
	uplinkLifecycle        UplinkLifecycle
	declarationDismissal   DeclarationDismissal
	issueLifecycle         IssueLifecycle
	schedulingObserver     SchedulingObserver
	runs                   RunResolver
	history                RunHistory
	repositories           RepositoryReader
	issues                 IssueRepository
	promotions             PromotionRepository
	promotionRuns          PromotionRunStore
	enqueues               EnqueueObserver
	git                    DeliveryGit
	logs                   LogStore
	artifacts              ArtifactStore
	refs                   RefResolver
	auditor                Auditor
	health                 Health
	signals                *Signals
	maximumWait            time.Duration
	waitClock              clock.Clock // nil uses real time; shared observation/long-poll clock
	mutationGate           func(context.Context) error
	promotionWorkspaceRoot string
	workspaces             *workspaceLifecycle
	secretAccess           SecretAccessStore

	releaseWIFOverrides       ReleaseWIFOverrideStore
	releaseWIF                ReleaseWIFPolicy
	releaseWIFMu              *sync.Mutex
	secretStoreVerifier       SecretStoreVerifier
	secretAccessReconciler    *AccessReconciler
	repositoryArchiver        RepositoryArchiver
	changeRepositoryArchive   func(context.Context, string, bool, func() error) error
	repositoryRemover         RepositoryRemover
	removeGitCache            func(string) error
	promotionGrants           PromotionGrantStore
	promotionGrantMaxLifetime time.Duration
	runCancels                RunCancelStore
	uplinks                   UplinkDirectory
	// background tracks automatic promotion retries (#1097); a pointer so
	// the per-actor copies share it.
	background *sync.WaitGroup
	// lifetime ends when Shutdown starts: background waits (a promotion
	// queued behind another, #1021) select on it, so shutdown cancels them
	// rather than waiting them out. Shared by the per-actor copies.
	lifetime     context.Context
	stopLifetime context.CancelFunc
	// closing refuses new background work once Shutdown began, so no
	// WaitGroup Add races its Wait and nothing starts against a store the
	// caller is about to close.
	closing   *backgroundClosing
	runReruns RunRerunStore
	// assistHolds and runAssists back the run_hold_* tools; nil disables them.
	assistHolds AssistHoldOperator
	runAssists  RunAssistStore
	assistGate  *assistGate
	retention   *Retention
	schedules   ScheduleControl
	// scope is set on the per-call copy serving a scoped actor (#073).
	scope *repoScope
	// agentEvents is shared by the per-actor copies (#1514).
	agentEvents *agentEventRecorder
}

func NewAPI(config APIConfig) (*API, error) {
	if config.IssueStaleAfter == 0 {
		config.IssueStaleAfter = DefaultIssueStaleAfter
	}
	if config.IssueActiveStaleAfter == 0 {
		config.IssueActiveStaleAfter = min(DefaultIssueActiveStaleAfter, config.IssueStaleAfter)
	}
	if config.IssueStaleAfter <= 0 || config.IssueActiveStaleAfter <= 0 || config.IssueActiveStaleAfter > config.IssueStaleAfter {
		return nil, fmt.Errorf("%w: issue stale windows must be positive and active must not exceed general", ErrInvalidInput)
	}

	if config.Runs == nil {
		return nil, errors.New("service: run resolver is required")
	}
	maximumWait := config.MaximumWait
	if maximumWait <= 0 || maximumWait > DefaultMaximumWait {
		maximumWait = DefaultMaximumWait
	}
	promotionWorkspaceRoot := config.PromotionWorkspaceRoot
	if strings.TrimSpace(promotionWorkspaceRoot) == "" {
		promotionWorkspaceRoot = "/data/work"
	}
	promotionWorkspaceRoot = filepath.Clean(promotionWorkspaceRoot)
	var workspaces *workspaceLifecycle
	if config.Promotions != nil {
		var err error
		workspaces, err = newWorkspaceLifecycle(promotionWorkspaceRoot, config.Promotions, config.RemoveWorkspace)
		if err != nil {
			return nil, err
		}
	}
	signals := config.Signals
	if signals == nil {
		signals = NewSignals()
	}
	mutationGate := config.MutationGate
	if mutationGate == nil {
		mutationGate = func(context.Context) error { return nil }
	}
	promotionGrantMaxLifetime := config.PromotionGrantMaxLifetime
	if promotionGrantMaxLifetime <= 0 {
		promotionGrantMaxLifetime = 24 * time.Hour
	}
	const promotionGrantHardCeiling = 168 * time.Hour // 7 days
	if promotionGrantMaxLifetime > promotionGrantHardCeiling {
		promotionGrantMaxLifetime = promotionGrantHardCeiling
	}
	lifetime, stopLifetime := context.WithCancel(context.Background())
	created := &API{
		administration:  config.Administration,
		adminBackupLock: &sync.Mutex{},
		branchPrune:     config.BranchPrune, branchDeletions: config.BranchDeletions, deletionQueued: config.DeletionQueued,
		issueStaleAfter: config.IssueStaleAfter, issueActiveStaleAfter: config.IssueActiveStaleAfter,
		releasePreflight:   config.ReleasePreflight,
		clientCertificates: config.ClientCertificates,
		uplinkLifecycle:    config.UplinkLifecycle, declarationDismissal: config.DeclarationDismissal,
		issueLifecycle:     config.IssueLifecycle,
		schedulingObserver: config.SchedulingObserver,
		branchCounts:       &branchCountCache{},
		runs:               config.Runs, history: config.History, repositories: config.Repositories,
		issues: config.Issues, promotions: config.Promotions, promotionRuns: config.PromotionRuns,
		tags: config.Tags, enqueues: config.Enqueues, git: config.Git, refs: config.Refs, logs: config.Logs, artifacts: config.Artifacts, auditor: config.Auditor,
		health: config.Health, signals: signals, maximumWait: maximumWait, mutationGate: mutationGate,
		promotionWorkspaceRoot: promotionWorkspaceRoot, workspaces: workspaces,
		secretAccess: config.SecretAccess, secretAccessReconciler: config.SecretAccessReconciler,
		workflowCapabilities: config.WorkflowCapabilities,

		secretStoreVerifier: config.SecretStoreVerifier,
		repositoryArchiver:  config.RepositoryArchiver, changeRepositoryArchive: config.ChangeRepositoryArchive,
		repositoryRemover: config.RepositoryRemover, removeGitCache: config.RemoveGitCache,
		promotionGrants: config.PromotionGrants, promotionGrantMaxLifetime: promotionGrantMaxLifetime,
		runCancels: config.RunCancels, retention: config.Retention, runReruns: config.RunReruns, schedules: config.Schedules, uplinks: config.Uplinks, background: &sync.WaitGroup{},
		assistHolds: config.AssistHolds, runAssists: config.RunAssists, assistGate: newAssistGate(maxConcurrentHoldActions),
		releaseWIFOverrides: config.ReleaseWIFOverrides, releaseWIF: config.ReleaseWIF, releaseWIFMu: &sync.Mutex{},

		promotionProgress: &sync.Map{},
		lifetime:          lifetime, stopLifetime: stopLifetime, closing: &backgroundClosing{},
	}
	created.agentEvents = newAgentEventRecorder(created, config.AgentEvents, config.AgentEventPolicy)
	return created, nil
}

// backgroundClosing guards the background WaitGroup against late Adds.
type backgroundClosing struct {
	mu     sync.Mutex
	closed bool
}

// startBackground registers one background goroutine, or reports false once
// Shutdown has begun; the caller then must not start it.
func (service *API) startBackground() bool {
	service.closing.mu.Lock()
	defer service.closing.mu.Unlock()
	if service.closing.closed {
		return false
	}
	service.background.Add(1)
	return true
}

// Shutdown cancels the API's background waits and waits, within ctx, for
// every background goroutine to return, so none touches the store after the
// caller closes it. A promotion whose queue wait is cancelled stays pending
// with no plan; owner-startup recovery interrupts it.
func (service *API) Shutdown(ctx context.Context) error {
	service.closing.mu.Lock()
	service.closing.closed = true
	service.closing.mu.Unlock()
	service.stopLifetime()
	done := make(chan struct{})
	go func() {
		service.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("service: background work still running at shutdown: %w", ctx.Err())
	}
}

func (service *API) CallTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (value any, resultErr error) {
	defer func() {
		if errors.Is(resultErr, store.ErrInvalid) && !errors.Is(resultErr, ErrInvalidInput) {
			resultErr = fmt.Errorf("%w: %w", ErrInvalidInput, resultErr)
		}
	}()
	if name == "run_rerun" || name == "publish_retry" {
		defer func() {
			if resultErr == nil {
				return
			}
			action, message := "", ""
			switch result := value.(type) {
			case api.RunRerunResponse:
				action, message = result.Action, result.Message
			case api.PublishRetryResponse:
				action, message = result.Action, result.Message
			}
			if action == "" || action == "none" {
				resultErr = &retryOutcomeError{cause: resultErr, outcome: "nothing started"}
			} else if message != "" {
				resultErr = &retryOutcomeError{cause: resultErr, outcome: message}
			}
		}()
	}
	ctx = model.WithAuthMethod(ctx, actor.AuthMethod)
	copy := *service
	service = &copy
	if name == "run_logs" || name == "logs" {
		ctx = withRunLogCaller(ctx, actor)
	}
	if name == "run_get" || name == "run_logs" || name == "logs" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runReadBudget)
		defer func() {
			if resultErr == nil {
				resultErr = ctx.Err()
			}
			resultErr = boundedRunReadError(resultErr)
			if resultErr != nil {
				value = nil
			}
			cancel()
		}()
	}
	if strings.TrimSpace(actor.Identity) == "" {
		return nil, errors.New("service: acting uplink identity is required")
	}
	if actor.MetricsOnly() {
		// The HTTP layer already refuses it; no tool is ever served to a
		// metrics-scoped uplink, whatever the caller (#1534).
		return nil, refuse(ErrForbidden, "a metrics-scoped token may read only GET /metrics")
	}
	// An admin may repair an issue's scope even when its own uplink has a
	// narrower read scope. An ordinary title/body edit keeps that scope.
	adminScopeMove := false
	if name == "issue_update" && actor.Admin {
		var arguments struct {
			Upstream *string `json:"upstream"`
		}
		adminScopeMove = json.Unmarshal(raw, &arguments) == nil && arguments.Upstream != nil
	}
	var err error
	if !adminScopeMove {
		service, err = service.forActor(ctx, actor)
	}
	if err != nil {
		return nil, err
	}
	value, err = service.dispatchTool(ctx, actor, name, raw)
	return service.stripScopedSequences(value), toolSelectorError(raw, err)
}

func (service *API) callTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if MutatingTool(name) {
		if err := service.requireMutation(ctx); err != nil {
			return nil, err
		}
	}
	switch name {
	case "upstream_add", "upstream_list", "upstream_remove", "upstream_provide_key", "repo_add", "repo_verify", "admin_access_list", "admin_public_identity", "admin_readiness", "admin_version", "admin_idle", "admin_audit_record", "uplink_scope", "uplink_add":
		return service.administrationTool(ctx, actor, name, raw)
	// Credential and grant lifecycle (#1105).
	case "uplink_invite":
		return service.uplinkInviteTool(ctx, actor, raw)
	case "uplink_cert", "client_renew":
		return service.clientCertTool(ctx, actor, name, raw)
	case "uplink_list", "uplink_revoke", "uplink_rotate":
		return service.uplinkLifecycleTool(ctx, actor, name, raw)
	case "access_dismiss":
		return service.dismissAccess(ctx, actor, raw)

	case "issue_unlock", "issue_close_many":
		return service.issueLifecycleTool(ctx, actor, name, raw)
	case "secretstore_verify", "secretstore_plan", "secretstore_sync_receipt":
		if err := requireUnscoped(actor, name); err != nil {
			return nil, err
		}
		switch name {
		case "secretstore_verify":
			return service.verifySecretStore(ctx, actor, raw)
		case "secretstore_plan":
			return service.secretStorePlan(ctx, actor, raw)
		default:
			return service.secretStoreSyncReceipt(ctx, actor, raw)
		}
	case "release_wif_list", "release_wif_grant", "release_wif_revoke", "release_wif_reset":
		return service.releaseWIFTool(ctx, actor, name, raw)
	case "audit_list":
		return service.auditList(ctx, actor, raw)
	case "schedule_list":
		return service.scheduleList(ctx, raw)
	case "schedule_pause", "schedule_resume":
		return service.schedulePauseResume(ctx, actor, name == "schedule_pause", raw)
	case "schedule_trigger":
		return service.scheduleTrigger(ctx, actor, raw)
	case "status":
		var arguments struct {
			Repo string `json:"repo"`
			Ref  string `json:"ref"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.Ref) == "" {
			return nil, fmt.Errorf("%w: ref is required", ErrInvalidInput)
		}
		return service.status(ctx, arguments.Repo, arguments.Ref, actor.Identity)
	case "logs":
		var arguments struct {
			Repo       string `json:"repo"`
			SHA        string `json:"sha"`
			Step       string `json:"step"`
			Pattern    string `json:"pattern"`
			Context    int    `json:"context"`
			Offset     int    `json:"offset"`
			Limit      int    `json:"limit"`
			Tail       bool   `json:"tail"`
			ByteOffset *int64 `json:"byte_offset"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
		if err != nil {
			return nil, err
		}
		if arguments.ByteOffset != nil && *arguments.ByteOffset < 0 {
			return nil, fmt.Errorf("%w: byte_offset must be nonnegative", ErrInvalidInput)
		}
		filter.ByteOffset = arguments.ByteOffset
		return service.logsFor(ctx, arguments.Repo, arguments.SHA, arguments.Step, filter)

	case "run_get":
		var arguments struct {
			ID      string `json:"id"`
			Summary *bool  `json:"summary"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" {
			return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
		}
		// The summary is the default; the full record (timing, diagnostics,
		// the repository record) only on an explicit summary:false.
		return service.readRun(ctx, actor, arguments.ID, arguments.Summary == nil || *arguments.Summary)
	case "publication_abandon":
		return service.abandonPublication(ctx, actor, raw)
	case "run_cancel":
		var arguments struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
			Force  bool   `json:"force"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.cancelRun(ctx, actor, arguments.ID, arguments.Reason, arguments.Force)
	case "run_rerun":
		var arguments struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
			Force  bool   `json:"force"`
			Assist bool   `json:"assist"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.rerunRun(ctx, actor, arguments.ID, arguments.Reason, arguments.Force, arguments.Assist)
	case "run_hold_list":
		var arguments struct {
			ID string `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.listRunHolds(ctx, actor, arguments.ID)
	case "run_hold_exec":
		var arguments struct {
			ID    string   `json:"id"`
			Step  string   `json:"step"`
			Argv  []string `json:"argv"`
			Stdin string   `json:"stdin"`
			timeoutArguments
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		timeout := arguments.seconds()
		return service.execRunHold(ctx, actor, arguments.ID, arguments.Step, arguments.Argv, arguments.Stdin, timeout)
	case "run_hold_patch":
		var arguments struct {
			ID   string `json:"id"`
			Step string `json:"step"`
			Diff string `json:"diff"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.patchRunHold(ctx, actor, arguments.ID, arguments.Step, arguments.Diff)
	case "run_continue":
		var arguments struct {
			ID   string `json:"id"`
			Step string `json:"step"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.continueRunHold(ctx, actor, arguments.ID, arguments.Step)
	case "run_hold_abandon":
		var arguments struct {
			ID   string `json:"id"`
			Step string `json:"step"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.abandonRunHold(ctx, actor, arguments.ID, arguments.Step)
	case "run_purge":
		if !actor.Admin || actor.Scoped() {
			return nil, refuse(ErrForbidden, "run_purge requires an unscoped admin uplink")
		}
		var arguments struct {
			ID     string `json:"id"`
			What   string `json:"what"`
			Reason string `json:"reason"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if service.retention == nil {
			return nil, refuse(ErrUnavailable, "retention")
		}
		value, err := service.retention.Purge(ctx, actor, arguments.ID, arguments.What, arguments.Reason)
		if errors.Is(err, store.ErrInvalid) {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		return value, err
	case "run_logs":
		var arguments struct {
			ID         string `json:"id"`
			Stage      string `json:"burn"`
			Step       string `json:"step"`
			Pattern    string `json:"pattern"`
			Context    int    `json:"context"`
			Offset     int    `json:"offset"`
			Limit      int    `json:"limit"`
			Tail       bool   `json:"tail"`
			ByteOffset *int64 `json:"byte_offset"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" {
			return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
		}
		filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
		if err != nil {
			return nil, err
		}
		if arguments.ByteOffset != nil && *arguments.ByteOffset < 0 {
			return nil, fmt.Errorf("%w: byte_offset must be nonnegative", ErrInvalidInput)
		}
		filter.ByteOffset = arguments.ByteOffset
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields) // decodeTool already validated the object.
		defaultWindow := true
		for field := range fields {
			switch strings.ToLower(field) {
			case "pattern", "limit", "tail", "offset", "byte_offset":
				defaultWindow = false
			}
		}
		if defaultWindow {
			filter.Tail, filter.Limit = true, runlog.DefaultWindowLines
		}
		value, err := service.RunLogFiltered(ctx, actor, arguments.ID, arguments.Stage, arguments.Step, filter)
		if err != nil {
			return nil, err
		}
		if response, ok := value.(LogResponse); ok && defaultWindow {
			response.Window = runlog.DefaultWindowDescription
			value = response
		}
		return value, nil
	case "artifacts":
		var arguments struct {
			ID string `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.RunArtifacts(ctx, actor, arguments.ID)
	case "artifact_get":
		var arguments struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Pattern string `json:"pattern"`
			Context int    `json:"context"`
			Offset  int    `json:"offset"`
			Limit   int    `json:"limit"`
			Tail    bool   `json:"tail"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" || strings.TrimSpace(arguments.Name) == "" {
			return nil, fmt.Errorf("%w: run ID and artifact name are required", ErrInvalidInput)
		}
		filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
		if err != nil {
			return nil, err
		}
		return service.RunArtifact(ctx, actor, arguments.ID, arguments.Name, filter)
	case "wait", "promote_status":
		prepared, err := service.PrepareLongPoll(ctx, actor, name, raw)
		if err != nil {
			return nil, err
		}
		return prepared(ctx)
	case "sync":
		var arguments struct {
			Repo   string `json:"repo"`
			SHA    string `json:"sha"`
			Branch string `json:"branch"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.sync(ctx, actor, arguments.Repo, arguments.SHA, arguments.Branch)
	case "promote":
		return service.promoteTool(ctx, actor, raw)
	case "promote_retry":
		var arguments struct {
			ID string `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.promoteRetry(ctx, actor, arguments.ID)
	case "promotion_list":
		var arguments promotionListArguments
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.promotionList(ctx, arguments)
	case "issue_create":
		var arguments struct {
			Title    string  `json:"title"`
			Body     *string `json:"body"`
			Upstream string  `json:"upstream"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if arguments.Body == nil {
			return nil, fmt.Errorf("%w: issue body is required", ErrInvalidInput)
		}
		issue, err := service.issueCreate(ctx, actor, arguments.Title, *arguments.Body, arguments.Upstream)
		if err != nil {
			return nil, err
		}
		return api.IssueCreateResponse{ID: issue.ID}, nil
	case "issue_get":
		var arguments struct {
			ID int64 `json:"id"`
			issueReadOptions
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if err := arguments.validate(); err != nil {
			return nil, err
		}
		service, err := service.withIssueObservation(arguments.issueObservationOptions)
		if err != nil {
			return nil, err
		}
		issue, err := service.issueGet(ctx, actor, arguments.ID)
		if err != nil {
			return nil, err
		}
		return arguments.project(issue), nil
	case "issue_get_many":
		var arguments struct {
			IDs []int64 `json:"ids"`
			issueReadOptions
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if err := arguments.validate(); err != nil {
			return nil, err
		}
		service, err := service.withIssueObservation(arguments.issueObservationOptions)
		if err != nil {
			return nil, err
		}
		return service.issueGetMany(ctx, actor, arguments.IDs, arguments.issueReadOptions)
	case "issue_update":
		var arguments struct {
			ID                 int64                  `json:"id"`
			WorkStatus         *model.IssueWorkStatus `json:"work_status"`
			Title              *string                `json:"title"`
			Body               *string                `json:"body"`
			Upstream           *string                `json:"upstream"`
			ExpectedBodySHA256 string                 `json:"expected_body_sha256"`
			ExpectedRevision   *int64                 `json:"expected_revision"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if value, ok := fields["expected_revision"]; ok && (string(value) == "null" || arguments.ExpectedRevision == nil || *arguments.ExpectedRevision < 1) {
			return nil, fmt.Errorf("%w: expected_revision must be a positive integer", ErrInvalidInput)
		}
		issue, err := service.issueUpdate(ctx, actor, arguments.ID, arguments.Title, arguments.Body, arguments.Upstream, arguments.ExpectedBodySHA256, arguments.ExpectedRevision, arguments.WorkStatus)
		if err != nil {
			return nil, err
		}
		return wireIssueMutation(issue, 0), nil
	case "issue_append":
		var arguments struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		issue, err := service.issueAppend(ctx, actor, arguments.ID, arguments.Text)
		if err != nil {
			return nil, err
		}
		return wireIssueMutation(issue, len(arguments.Text)), nil
	case "issue_close":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		issue, err := service.issueClose(ctx, actor, arguments.ID)
		if err != nil {
			return nil, err
		}
		return wireIssueMutation(issue, 0), nil
	case "issue_reopen":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if service.issues == nil || arguments.ID <= 0 {
			return nil, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
		}
		issue, err := service.issues.ReopenManualIssue(ctx, actor.Identity, arguments.ID)
		if err != nil {
			// Actor and ID were checked above. The store's invalid result is
			// the CI-policy refusal; keep unrelated store failures opaque.
			if errors.Is(err, store.ErrInvalid) {
				return nil, fmt.Errorf("%w: only manual issues can be reopened; CI state follows runs", ErrInvalidInput)
			}
			return nil, err
		}
		return wireIssue(issue), nil
	case "issue_delete":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if err := service.issueDelete(ctx, actor, arguments.ID); err != nil {
			return nil, err
		}
		return api.IssueDeleteResponse{ID: arguments.ID, Deleted: true}, nil
	case "issue_list":
		var arguments struct {
			Repo       string `json:"repo"`
			WorkStatus string `json:"work_status"`
			issueObservationOptions
			Kind   string `json:"kind"`
			State  string `json:"state"`
			Query  string `json:"query"`
			Before int64  `json:"before"`
			Limit  *int   `json:"limit"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		limit := maximumIssuePage
		if arguments.Limit != nil {
			limit = *arguments.Limit
			if limit < 1 || limit > maximumIssuePage {
				return nil, fmt.Errorf("%w: issue limit must be between 1 and 50", ErrInvalidInput)
			}
		}
		if arguments.State == "all" {
			arguments.State = ""
		}
		service, err := service.withIssueObservation(arguments.issueObservationOptions)
		if err != nil {
			return nil, err
		}
		page, err := service.issueList(ctx, arguments.Repo, arguments.Kind, arguments.State, arguments.Query, arguments.Before, limit, arguments.WorkStatus)
		if err != nil {
			return nil, err
		}
		return wireIssueList(page), nil
	case "issue_lock":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if service.issues == nil {
			return nil, refuse(ErrUnavailable, "issues")
		}
		lock, err := service.issues.AcquireIssueLock(ctx, arguments.ID, actor.Identity)
		if err != nil {
			return nil, err
		}
		return wireIssueLock(lock), nil
	case "access_list":
		var arguments struct {
			Repo    string `json:"repo"`
			Revoked bool   `json:"revoked"`
			Pending bool   `json:"pending"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if arguments.Pending {
			return service.accessListPending(ctx, arguments.Repo)
		}
		return service.accessList(ctx, arguments.Repo, arguments.Revoked)
	case "workflow_capability_grant":
		var arguments struct {
			Repo       string                   `json:"repo"`
			Capability *argoworkflow.Capability `json:"capability"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if arguments.Capability == nil {
			return nil, fmt.Errorf("%w: capability is required", ErrInvalidInput)
		}
		return service.workflowCapabilityTool(ctx, actor, name, arguments.Repo, arguments.Capability)
	case "workflow_capability_revoke", "workflow_capability_get":
		var arguments struct {
			Repo string `json:"repo"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.workflowCapabilityTool(ctx, actor, name, arguments.Repo, nil)

	case "access_allow":
		var arguments struct {
			Repo     string `json:"repo"`
			Workflow string `json:"workflow"`
			Step     string `json:"step"`
			Secret   string `json:"secret"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.accessAllow(ctx, actor, arguments.Repo, arguments.Workflow, arguments.Step, arguments.Secret)
	case "access_revoke":
		var arguments struct {
			Repo     string `json:"repo"`
			Workflow string `json:"workflow"`
			Step     string `json:"step"`
			Secret   string `json:"secret"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.accessRevoke(ctx, actor, arguments.Repo, arguments.Workflow, arguments.Step, arguments.Secret)
	case "repo_list":
		var arguments struct {
			Archived string `json:"archived"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.repoList(ctx, arguments.Archived)
	case "repo_archive", "repo_unarchive":
		var arguments struct {
			Repo string `json:"repo"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.repoArchive(ctx, actor, arguments.Repo, name == "repo_archive")
	case "branch_delete_many":
		return service.branchDeleteMany(ctx, actor, raw)
	case "branch_deletion_list":
		return service.branchDeletionList(ctx, actor, raw)
	case "ref_delete_many":
		return service.refDeleteMany(ctx, actor, raw)
	case "branch_deletion_resolve":
		return service.branchDeletionResolve(ctx, actor, raw)
	case "receive_outbox":
		return service.receiveOutboxTool(ctx, actor, raw)
	case "repo_remove":
		var arguments struct {
			Repo string `json:"repo"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.repoRemove(ctx, actor, arguments.Repo)
	case "run_list":
		var arguments struct {
			Repo       string                `json:"repo"`
			Status     model.RunStatusFilter `json:"status"`
			Superseded bool                  `json:"superseded"`
			Ref        string                `json:"ref"`
			Limit      int                   `json:"limit"`
			Before     pageCursor            `json:"before"`
			View       string                `json:"view"`
			Own        bool                  `json:"own"`
			Held       bool                  `json:"held"`
			RefGlob    string                `json:"ref_glob"`
			Latest     bool                  `json:"latest"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		owner := ""
		if arguments.Own {
			owner = actor.Identity
		}
		if arguments.Held {
			if arguments.Latest || arguments.RefGlob != "" {
				return nil, fmt.Errorf("%w: held:true is one unpaged list; it takes no latest or ref_glob", ErrInvalidInput)
			}
			if arguments.Superseded {
				return nil, fmt.Errorf("%w: held:true and superseded:true select different runs; pass one", ErrInvalidInput)
			}
			runs, err := service.heldRuns(ctx, api.RunFilter{Status: arguments.Status, Repository: arguments.Repo, Ref: arguments.Ref, View: arguments.View,
				Before: arguments.Before.Sequence, BeforeRun: arguments.Before.ID, Actor: owner})
			if err != nil {
				return nil, err
			}
			return api.RunListResponse{Runs: runs}, nil
		}
		limit := arguments.Limit
		if limit <= 0 {
			limit = 50
		}
		// The store pages at most 100 runs; the old "max 200" was silently
		// clipped to 100 with no way to reach older runs (#050).
		if limit > 100 {
			limit = 100
		}
		runs, err := service.Runs(ctx, actor, api.RunFilter{Status: arguments.Status, Superseded: arguments.Superseded, Repository: arguments.Repo, Ref: arguments.Ref, Limit: limit,
			RefGlob: strings.TrimSpace(arguments.RefGlob), LatestPerRef: arguments.Latest,
			Before: arguments.Before.Sequence, BeforeRun: arguments.Before.ID, View: arguments.View, Actor: owner})
		if err != nil {
			return nil, err
		}
		response := api.RunListResponse{Runs: runs}
		if page, ok := runs.([]model.Run); ok && api.RunPageFull(page, api.RunFilter{Limit: limit, View: arguments.View}) {
			// A scoped uplink pages by run ID: a global sequence would
			// count every run on the installation (#073).
			if service.scope != nil {
				response.NextCursor = page[len(page)-1].ID
			} else {
				response.NextBefore = page[len(page)-1].QueueSequence
			}
		}
		return response, nil
	case "publish_retry":
		var arguments struct {
			ID string `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" {
			return nil, fmt.Errorf("%w: run or promotion ID is required", ErrInvalidInput)
		}
		return service.publishRetry(ctx, actor, arguments.ID)
	case "system_status":
		return service.Status(ctx, actor)
	case "promote_grant":
		return service.promoteGrant(ctx, actor, raw)
	case "promote_grant_revoke":
		return service.promoteGrantRevoke(ctx, actor, raw)
	case "promote_grant_list":
		return service.promoteGrantList(ctx, actor, raw)
	default:
		return nil, fmt.Errorf("%w: unknown tool %q", ErrInvalidInput, name)
	}
}

func (service *API) Runs(ctx context.Context, actor api.Actor, filter api.RunFilter) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if service.history == nil {
		return nil, refuse(ErrUnavailable, "run history")
	}
	if err := filter.Status.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if !api.ValidRunView(filter.View) {
		return nil, fmt.Errorf("%w: view must be all or history", ErrInvalidInput)
	}
	if filter.LatestPerRef && filter.View != "" && filter.View != api.RunViewAll {
		return nil, fmt.Errorf("%w: latest_per_ref requires all view", ErrInvalidInput)
	}

	if filter.Before < 0 || service.scope != nil && filter.Before > 0 && filter.BeforeRun == "" {
		return nil, fmt.Errorf("%w: before must be a positive run sequence, or a run ID for a scoped uplink", ErrInvalidInput)
	}
	if err := validRefGlob(filter.RefGlob); err != nil {
		return nil, err
	}
	storeFilter := model.RunListFilter{Status: filter.Status, Superseded: filter.Superseded, LatestPerRef: filter.LatestPerRef, Ref: filter.Ref, RefGlob: filter.RefGlob, Limit: filter.Limit, Before: filter.Before, Terminal: filter.View == api.RunViewHistory, Actor: filter.Actor}
	if strings.TrimSpace(filter.BeforeRun) != "" {
		// A cursor that is both a sequence and a run-ID spelling is a run
		// first; an unscoped caller falls back to the sequence (#073).
		cursor, err := service.resolveReadRunID(ctx, filter.BeforeRun)
		switch {
		case err == nil:
			if filter.LatestPerRef || filter.View == api.RunViewHistory {
				storeFilter.Before = cursor.QueueSequence
			} else {
				storeFilter.Before, storeFilter.BeforeRun = 0, cursor.ID
			}
		case service.scope == nil && filter.Before > 0 && (errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrInvalidInput)):
		case filter.View != api.RunViewHistory && errors.Is(err, store.ErrNotFound):
			return nil, store.ErrRunCursorNotFound
		default:
			return nil, err
		}
	}
	if strings.TrimSpace(filter.Repository) != "" {
		repository, err := service.runs.RepositoryByName(ctx, filter.Repository)
		if err != nil {
			return nil, err
		}
		storeFilter.RepoID = repository.ID
	}
	runs, err := service.history.ListRecentRuns(ctx, storeFilter)
	if err != nil {
		return nil, err
	}
	runs, err = service.presentationRuns(ctx, runs)
	if err != nil {
		return nil, err
	}
	for i := range runs {
		if filter.View == api.RunViewHistory {
			continue
		}
		switch runs[i].DisplayStatus {
		case model.DisplayRunning:
			runs[i].Scheduling = service.observeRunListExecution(ctx, runs[i])
			applyExecutionDisplay(&runs[i], runs[i].Scheduling)
			if runs[i].DisplayReason == "" && (runs[i].Scheduling == nil || runs[i].Scheduling.Execution == nil || runs[i].Scheduling.Execution.State == "unavailable" || runs[i].Scheduling.Execution.State == "not_observed") {
				runs[i].DisplayReason = "awaiting Argo state"
			}
		case model.DisplayQueued:
			var reason string
			runs[i].Scheduling, reason = service.observeScheduling(ctx, runs[i], nil, false)
			applyQueuedDisplay(&runs[i], runs[i].Scheduling, reason)
		}
	}
	return service.stripScopedSequences(runs), nil
}

// RunCounts is the dashboard's count of retained runs, in total and per
// repository ID; the run list itself is paged.
func (service *API) RunCounts(ctx context.Context, actor api.Actor) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if service.history == nil {
		return nil, refuse(ErrUnavailable, "run history")
	}
	return service.history.CountRuns(ctx)
}

// Grants is the read-only dashboard/API view of promotion grants, mirroring
// the MCP promote_grant_list tool with query-string filters. No mutation
// endpoints exist; mutations stay MCP-only, admin-only.
func (service *API) Grants(ctx context.Context, actor api.Actor, filter api.GrantFilter) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if service.promotionGrants == nil || service.runs == nil {
		return nil, refuse(ErrUnavailable, "promotion grants")
	}
	grants, err := service.promotionGrantRows(ctx, filter.Repository, filter.SHA, filter.Status)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0, len(grants))
	for _, grant := range grants {
		result = append(result, promotionGrantResponse(grant))
	}
	return result, nil
}

// resolveReadRunID accepts the unique ID prefix emitted by push feedback.
// Writers receive the resolved full ID and keep their exact identity lookups.
func (service *API) resolveReadRunID(ctx context.Context, id string) (model.Run, error) {
	run, err := service.history.ResolveRunID(ctx, id)
	if errors.Is(err, store.ErrInvalid) {
		return model.Run{}, fmt.Errorf("%w: run ID must be 8 to 32 hexadecimal characters", ErrInvalidInput)
	}
	return run, err
}

// Run is the read-only dashboard view of one exact run: the durable record,
// its recorded step results, and the owning repository. Unlike the MCP status
// tool it resolves by run ID, never renews an issue lock, and has no other
// side effects, so an idle dashboard cannot hold agent coordination state.
func (service *API) Run(ctx context.Context, actor api.Actor, id string) (any, error) {
	return service.readRun(ctx, actor, id, false)
}

func (service *API) readRun(ctx context.Context, actor api.Actor, id string, summary bool) (value any, resultErr error) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, runReadBudget)
	defer func() {
		if resultErr == nil {
			resultErr = ctx.Err()
		}
		resultErr = boundedRunReadError(resultErr)
		if resultErr != nil {
			value = nil
		}
		cancel()
	}()

	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if service.history == nil {
		return nil, refuse(ErrUnavailable, "run history")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return nil, err
	}
	steps, err := service.stepsForRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("load run steps: %w", err)
	}
	if summary {
		shown := service.presentationRun(ctx, run)
		run.Repo, err = service.repositoryIdentity(ctx, run.RepoID)
		if err != nil {
			return nil, err
		}
		run.DisplayStatus, run.DisplayReason = shown.DisplayStatus, shown.DisplayReason
		summarized := summarizeRun(run, steps)
		// The compact summary keeps the hint's class, cause and action.
		if hint := runRetryHint(run, service.runPublicationRetry(ctx, run)); hint != nil {
			hint.Detail = ""
			summarized.RetryHint = hint
		}
		return summarized, nil
	}
	scheduling, queuedReason := service.observeScheduling(ctx, run, steps, true)
	response := RunDetailResponse{Run: service.presentationRun(ctx, run), Steps: steps, Scheduling: scheduling,
		RerunOf: service.runRerunOf(ctx, run.ID)}
	response.RetryHint = runRetryHint(response.Run, service.runPublicationRetry(ctx, run))
	applyExecutionDisplay(&response.Run, response.Scheduling)
	if reader, ok := service.history.(RunTimingReader); ok {
		timing, timingErr := reader.RunTiming(ctx, run.ID)
		if timingErr != nil {
			return nil, fmt.Errorf("load run timing: %w", timingErr)
		}
		response.Timing = &timing
	}
	applyQueuedDisplay(&response.Run, response.Scheduling, queuedReason)
	if service.retention != nil && run.Status.Terminal() {
		response.Purges, err = service.retention.Database.RunEvidencePurges(ctx, run.ID)
		if err != nil {
			return nil, err
		}
	}
	if service.repositories != nil {
		repository, repositoryErr := service.repositories.Repository(ctx, run.RepoID)
		if repositoryErr != nil && !errors.Is(repositoryErr, store.ErrNotFound) {
			return nil, fmt.Errorf("load run repository: %w", repositoryErr)
		}
		if repositoryErr == nil {
			response.Repository = repository
		}
	}
	return service.stripScopedSequences(response), nil
}

// RunLog is the read-only dashboard view of one known step's bounded redacted
// log. Active runs authorize against their progress journal and read an
// in-memory index snapshot; terminal runs authorize against durable results
// and their finalized retained-log index.
func (service *API) RunLog(ctx context.Context, actor api.Actor, id, stage, step string) (any, error) {
	return service.RunLogFiltered(ctx, actor, id, stage, step, runlog.Filter{})
}

func (service *API) RunLogFiltered(ctx context.Context, actor api.Actor, id, stage, step string, filter runlog.Filter) (value any, resultErr error) {
	var availablePairs []string
	ctx = withRunLogCaller(ctx, actor)
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, runReadBudget)
	defer func() {
		if resultErr == nil {
			resultErr = ctx.Err()
		}
		resultErr = boundedRunReadError(resultErr)
		if resultErr != nil {
			if len(availablePairs) != 0 {
				resultErr = fmt.Errorf("%w; available burn/step pairs: %s", resultErr, strings.Join(availablePairs, ", "))
			}
			value = nil
		}
		cancel()
	}()

	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if service.logs == nil || service.history == nil {
		return nil, refuse(ErrUnavailable, "logs")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return nil, err
	}
	steps, err := service.stepsForRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("load run steps: %w", err)
	}
	if stage == "" && step == "" && run.Status == model.RunPassed && len(steps) > 0 {
		stage, step = steps[0].Stage, steps[0].Step
		for _, result := range steps {
			availablePairs = append(availablePairs, result.Stage+"/"+result.Step)
		}
	}
	if strings.TrimSpace(stage) == "" {
		if strings.TrimSpace(step) != "" {
			stages := map[string]bool{}
			for _, result := range steps {
				if result.Step == step {
					stages[result.Stage] = true
				}
			}
			candidates := make([]string, 0, len(stages))
			for candidate := range stages {
				candidates = append(candidates, candidate)
			}
			sort.Strings(candidates)
			switch len(candidates) {
			case 0:
				return nil, fmt.Errorf("%w: no recorded step %q in this run", store.ErrNotFound, step)
			case 1:
				stage = candidates[0]
			default:
				return nil, fmt.Errorf("%w: step %q matches burns: %s; specify burn", ErrInvalidInput, step, strings.Join(candidates, ", "))
			}
		} else {
			stage, step = run.FailedStage, run.FailedStep
		}
	}
	if strings.TrimSpace(stage) == "" {
		pairs := make([]string, 0, len(steps))
		for _, result := range steps {
			pairs = append(pairs, result.Stage+"/"+result.Step)
		}
		sort.Strings(pairs)
		available := strings.Join(pairs, ", ")
		if available == "" {
			available = "none recorded"
		}
		return nil, fmt.Errorf("%w: no failed step; specify burn and step; available burn/step pairs: %s", ErrInvalidInput, available)
	}
	if strings.TrimSpace(step) == "" {
		var count int
		for _, result := range steps {
			if result.Stage == stage {
				step = result.Step
				count++
			}
		}
		if count != 1 {
			return nil, fmt.Errorf("%w: step is required unless burn %q has exactly one step", ErrInvalidInput, stage)
		}
	}
	recorded := false
	for _, result := range steps {
		if result.Stage == stage && result.Step == step {
			recorded = true
			break
		}
	}
	if !recorded {
		return nil, fmt.Errorf("no log range for burn %q step %q: %w", stage, step, store.ErrNotFound)
	}
	response, err := service.stepLogResponse(ctx, run, steps, stage, step, filter)
	response.AvailablePairs = availablePairs
	return response, err
}

// maxLiveLogChunk bounds one live-log poll response. 256 KiB keeps a fast
// producer from turning a dashboard poll into a multi-megabyte transfer while
// still draining bursts within a couple of poll cycles.
const maxLiveLogChunk = 256 << 10

// LiveLogSize is an internal stream hint, used only after an authorized live
// read. It confers no read authority; streams still refresh the full view roughly
// once a second for terminal state, scope and evidence retention checks.
func (service *API) LiveLogSize(id string) (int64, error) {
	if source, ok := service.logs.(interface{ LiveLogSize(string) (int64, error) }); ok {
		return source.LiveLogSize(id)
	}
	return 0, ErrUnavailable
}

// RunLogLive is the read-only polled live view of a run's redacted log
// stream. The run's terminal state is resolved BEFORE the chunk is read, so
// Done=true can never be reported while unread bytes remain: a run finishing
// between the two reads surfaces as one more running poll, and the client's
// next call drains the remainder with Done set. The underlying file receives
// only the secret-masked stream (masking happens in the Job controller before
// any byte reaches the log writer), so this view exposes exactly what the
// retained per-step view exposes — earlier.
func (service *API) RunLogLive(ctx context.Context, actor api.Actor, id string, offset int64) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if service.logs == nil || service.history == nil {
		return nil, refuse(ErrUnavailable, "logs")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := service.purgedEvidence(ctx, run, "logs"); err != nil {
		return nil, err
	}
	terminal := run.Status.Terminal()
	presented := service.presentationRun(ctx, run)
	if presented.DisplayStatus == model.DisplayQueued {
		scheduling, reason := service.observeScheduling(ctx, run, nil, true)
		applyQueuedDisplay(&presented, scheduling, reason)
	}
	response := LiveLogResponse{RunID: run.ID, Status: string(visibleRun(run).Status), DisplayStatus: presented.DisplayStatus, DisplayReason: presented.DisplayReason}
	// Status and purge checks above still run on idle refreshes, but an
	// unchanged append-only log needs no open/read even when it just finished.
	if offset >= 0 {
		if size, err := service.LiveLogSize(run.ID); err == nil && size == offset {
			response.Offset, response.Size, response.Done = size, size, terminal
			return response, nil
		}
	}
	chunk, next, size, err := service.logs.ReadFrom(run.ID, offset, maxLiveLogChunk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The log file appears when the Job starts streaming; a queued or
			// just-admitted run legitimately has none yet. A terminal run with
			// no file is genuinely done.
			response.Done = terminal
			return response, nil
		}
		return nil, fmt.Errorf("read live run log: %w", err)
	}
	response.Chunk = string(chunk)
	response.Offset = next
	response.Size = size
	// Done is true only when the run is terminal AND the read has reached
	// the end of the file. This prevents reporting done while unread bytes
	// remain in a multi-chunk terminal log.
	response.Done = terminal && next >= size
	return response, nil
}

func (service *API) Repositories(ctx context.Context, actor api.Actor) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	var repos []model.Repository
	if service.repositories != nil {
		repos, err = service.repositories.ListRepositories(ctx)
	} else {
		repos, err = service.runs.ListRepositories(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Fetch the latest runs per repo so the dashboard never derives per-repo
	// status from a global top-N window that silently drops quiet repos.
	var runsByRepo map[int64][]model.Run
	if service.history != nil {
		runs, historyErr := service.history.ListLatestRunsPerRepo(ctx, 12)
		if historyErr == nil {
			runs, historyErr = service.presentationRuns(ctx, runs)
		}
		if historyErr == nil {
			runsByRepo = make(map[int64][]model.Run, len(repos))
			for _, run := range runs {
				runsByRepo[run.RepoID] = append(runsByRepo[run.RepoID], run)
			}
		}
	}
	branches := service.nonDefaultBranchCounts(ctx, repos)
	views := make([]RepoView, len(repos))
	for i, repo := range repos {
		views[i] = RepoView{Repository: repo, LatestRuns: runsByRepo[repo.ID], NonDefaultBranches: branchCountPointer(branches, repo.ID)}
	}
	return service.stripScopedSequences(views), nil
}

func (service *API) Issues(ctx context.Context, actor api.Actor, filter api.IssueFilter) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	service, err = service.withIssueObservation(issueObservationOptions{StaleAfter: filter.StaleAfter, ActiveStaleAfter: filter.ActiveStaleAfter})
	if err != nil {
		return nil, err
	}
	page, err := service.issueList(ctx, filter.Repository, filter.Kind, filter.State, filter.Query, int64(filter.BeforeID), filter.Limit, filter.WorkStatus)
	if err != nil {
		return nil, err
	}
	result := api.IssueListPage{Issues: make([]api.IssueListSummary, len(page.Issues)), NextBefore: page.NextBefore, Total: page.Total, Counts: page.Counts}
	for i, issue := range page.Issues {
		result.Issues[i] = api.IssueListSummary{
			ID:         issue.ID,
			WorkStatus: issue.WorkStatus, Stale: issue.Stale,
			Revision:    issue.Revision,
			RepoID:      issue.RepoID,
			Kind:        issue.Kind,
			Branch:      issue.Branch,
			Title:       issue.Title,
			State:       issue.State,
			Occurrences: issue.Occurrences,
			CIOrigin:    issue.CIOrigin,
			CIWorkID:    issue.CIWorkID,
			Upstream:    issue.Upstream,
			Lock:        issue.Lock,
			CreatedAt:   issue.CreatedAt,
			UpdatedAt:   issue.UpdatedAt,
			ClosedAt:    issue.ClosedAt,
		}
	}
	return result, nil
}

func (service *API) Status(ctx context.Context, actor api.Actor) (any, error) {
	if service.health == nil {
		return nil, refuse(ErrUnavailable, "status")
	}
	if actor.Scoped() {
		// Installation-wide status counts every upstream and repository;
		// a scoped actor gets the scoped view or nothing (#073).
		scoped, ok := service.health.(ScopedHealth)
		if !ok {
			return nil, refuse(ErrUnavailable, "scoped status")
		}
		return scoped.ScopedStatus(ctx, actor)
	}
	return service.health.Status(ctx)
}

func (service *API) Ready(ctx context.Context) error {
	if service.health == nil {
		return refuse(ErrUnavailable, "readiness")
	}
	return service.health.Ready(ctx)
}

func (service *API) status(ctx context.Context, repositoryName, selector, actor string) (StatusResponse, error) {
	selector = strings.TrimSpace(selector)
	repository, run, runErr := service.resolveRun(ctx, repositoryName, selector)
	if runErr != nil && !errors.Is(runErr, store.ErrNotFound) {
		return StatusResponse{}, runErr
	}
	// Preserve SHA precedence and tag/ref ambiguity from the ordinary
	// resolver. Branch status, however, belongs to the current cached HEAD,
	// even when an older run was recorded under that branch's name.
	selectsSHA := runErr == nil && len(selector) >= 7 && strings.HasPrefix(strings.ToLower(run.SHA), strings.ToLower(selector))
	if repository.ArchivedAt == nil && service.refs != nil && !selectsSHA && !strings.HasPrefix(selector, "refs/tags/") &&
		(runErr != nil || run.RefKind == model.RefBranch) {
		response, refErr := service.statusForBranchRef(ctx, repositoryName, selector, actor)
		if refErr == nil && runErr == nil && response.Repository.ID != repository.ID {
			return StatusResponse{}, service.ambiguousRepositories(ctx, selector, []model.Repository{repository, response.Repository})
		}
		// A missing or unreadable current ref is not evidence that its old
		// branch run is current. Historical evidence remains addressable by SHA.
		return response, refErr
	}
	if runErr != nil {
		return StatusResponse{}, runErr
	}
	return service.statusForRun(ctx, repository, run)
}

func (service *API) statusForRun(ctx context.Context, repository model.Repository, run model.Run) (StatusResponse, error) {
	presented := service.presentationRun(ctx, run)
	response := StatusResponse{

		Repository: repository, Run: run, Repo: repository.Name, Ref: run.Ref,
		SHA: run.SHA, RunID: run.ID, Status: wireRunStatus(visibleRun(run).Status), Stages: map[string]string{},
		DisplayStatus:   presented.DisplayStatus,
		DisplayReason:   presented.DisplayReason,
		ResourceMessage: presented.ResourceMessage,
		ScheduleEntry:   run.ScheduleEntry,
	}
	if service.history != nil {
		steps, err := service.stepsForRun(ctx, run)
		if err != nil {
			return StatusResponse{}, fmt.Errorf("load run steps: %w", err)
		}
		response.Steps = steps
		response.Stages, response.ExitCode = wireStages(response.Steps, run)
	}
	var queuedReason string
	response.Scheduling, queuedReason = service.observeScheduling(ctx, run, response.Steps, true)
	response.Run = presented
	applyExecutionDisplay(&response.Run, response.Scheduling)
	applyQueuedDisplay(&response.Run, response.Scheduling, queuedReason)
	response.DisplayStatus, response.DisplayReason = response.Run.DisplayStatus, response.Run.DisplayReason
	response.FailedStep = run.FailedStep
	if service.issues != nil && run.RefKind == model.RefBranch {
		issue, issueErr := service.issues.OpenCIIssue(ctx, repository.ID, run.Ref)
		if issueErr != nil && !errors.Is(issueErr, store.ErrNotFound) {
			return StatusResponse{}, fmt.Errorf("load run issue: %w", issueErr)
		}
		if issueErr == nil {
			issueID := issue.ID
			response.Issue = &issueID
		}
	}
	return response, nil
}

// upstreamResolver is the optional store capability statusForBranchRef uses
// to compose fully-qualified cache inputs. The production store implements
// it; fixtures without upstream wiring fall back to the bare name, which
// resolves for every uniquely-named repository (issue #264).
type upstreamResolver interface {
	Upstream(context.Context, int64) (model.Upstream, error)
}

// cacheInputFor returns the repository input used for git-cache reads:
// fully qualified when the upstream is resolvable, bare otherwise. A bare
// name that is ambiguous across upstreams makes the cache read fail closed
// rather than guess.
func (service *API) cacheInputFor(ctx context.Context, repository model.Repository) string {
	if resolver, ok := service.runs.(upstreamResolver); ok {
		if upstream, err := resolver.Upstream(ctx, repository.UpstreamID); err == nil {
			return upstream.QualifiedRepo(repository.Name)
		}
	}
	return repository.Name
}

// statusForBranchRef reads the current branch HEAD from the bare Git cache,
// then selects its newest exact-SHA run across source and promotion refs.
// An untested HEAD returns no-runs, never historical branch evidence.
func (service *API) statusForBranchRef(ctx context.Context, repositoryName, selector, actor string) (StatusResponse, error) {
	if strings.TrimSpace(repositoryName) != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return StatusResponse{}, err
		}
		sha, err := service.refs.RefSHA(ctx, service.cacheInputFor(ctx, repository), strings.TrimPrefix(selector, "refs/heads/"))
		if err != nil {
			return StatusResponse{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
		}
		run, runErr := service.runs.ResolveRun(ctx, repository.ID, sha)
		if runErr == nil && sameOID(run.SHA, sha) {
			return service.statusFromRun(ctx, repository, run, selector, actor)
		}
		if runErr != nil && !errors.Is(runErr, store.ErrNotFound) {
			return StatusResponse{}, runErr
		}
		return noRunBranchStatus(repository, selector, sha), nil
	}
	repositories, err := service.runs.ListRepositories(ctx)
	if err != nil {
		return StatusResponse{}, err
	}
	var matched model.Repository
	var matchedSHA string
	var matches []model.Repository
	for _, repository := range repositories {
		sha, refErr := service.refs.RefSHA(ctx, service.cacheInputFor(ctx, repository), strings.TrimPrefix(selector, "refs/heads/"))
		if refErr != nil {
			continue
		}
		matches = append(matches, repository)
		matched, matchedSHA = repository, sha
	}
	if len(matches) > 1 {
		return StatusResponse{}, service.ambiguousRepositories(ctx, selector, matches)
	}
	if matched.ID == 0 {
		return StatusResponse{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
	}
	run, runErr := service.runs.ResolveRun(ctx, matched.ID, matchedSHA)
	if runErr == nil && sameOID(run.SHA, matchedSHA) {
		return service.statusFromRun(ctx, matched, run, selector, actor)
	}
	if runErr != nil && !errors.Is(runErr, store.ErrNotFound) {
		return StatusResponse{}, runErr
	}
	return noRunBranchStatus(matched, selector, matchedSHA), nil
}

// statusFromRun builds a full StatusResponse from an already-resolved
// repository and run, overriding the response ref with the caller's original
// selector (so "status main" shows ref "main" even when the run's own ref is
// a promotion ref like "promotion/main/abcdef012345").
func (service *API) statusFromRun(ctx context.Context, repository model.Repository, run model.Run, ref, _ string) (StatusResponse, error) {
	response, err := service.statusForRun(ctx, repository, run)
	response.Ref = ref
	return response, err
}

func wireRunStatus(status model.RunStatus) string {
	if status == model.RunPassed {
		return "delivered"
	}
	return string(status)
}

// visibleRun keeps the legacy wire projection: a claimed run that has not
// started reports Status waiting. Its display_status is queued, from
// model.DisplayRunStatus, the one raw-to-display mapping.
func visibleRun(run model.Run) model.Run {
	if run.ClaimedUnstarted() {
		run.Status = model.RunWaiting
	}
	return run
}

func presentationRun(run model.Run) model.Run {
	return presentationRunAt(run, time.Now())
}

func presentationRunAt(run model.Run, now time.Time) model.Run {
	visible := visibleRun(run)
	var collectionDeadline bool
	run.Error, collectionDeadline = model.SplitArtifactCollectionDeadline(run.Error)
	visible.DisplayStatus = model.DisplayRunStatus(run)
	visible.DisplayReason = runDisplayReason(run)
	if visible.DisplayStatus == model.DisplayQueued {
		visible.DisplayReason = queuedDisplayReason(nil)
	}
	if run.Status == model.RunRunning && isPendingCreate(run) {
		visible.DisplayReason = "workflow create uncertain, cleanup pending"
	}
	if run.Status == model.RunRunning && run.Phase == "publishing" {
		visible.DisplayReason = "publishing to upstream"
	}

	if visible.DisplayStatus != model.DisplayQueued {
		// Keep bounded eviction/reboot evidence and the retry link intact.
		if evictionDisplayReason(run) == "" && nodeRebootDisplayReason(run) == "" {
			visible.DisplayReason = displayFailureText(visible.DisplayReason)
		}
		visible.DisplayReason = parkPolicyDisplayReason(run, visible.DisplayReason)
		if (run.Status == model.RunPassed || run.Status == model.RunFailed) && collectionDeadline {
			visible.DisplayReason = joinRunMessage(visible.DisplayReason, model.ArtifactCollectionDeadlineReason)
		}
		// The assisted fragment stays whole: its digest and actor are the
		// evidence trail, so it is appended after the bounded clipping.
		if reason := assistDisplayReason(run); reason != "" {
			visible.DisplayReason = joinRunMessage(visible.DisplayReason, reason)
		}
	}
	return visible
}

func joinRunMessage(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + " · " + second
}

func runDisplayReason(run model.Run) string {
	if run.Status == model.RunPassed {
		return passedRunDisplayReason(run)
	}
	if run.Status == model.RunFailed {
		return failedRunDisplayReason(run)
	}
	if run.Status != model.RunInterrupted {
		return ""
	}
	switch {
	case run.Phase == model.PhaseCancelled:
		return strings.TrimPrefix(run.Reason, "cancelled ")
	case run.SupersededBy != "" && run.Reason == "superseded by newer run":
		return "superseded by newer run"
	default:
		return "server restart, retry needed"
	}
}

func (service *API) presentationRun(ctx context.Context, run model.Run) (result model.Run) {
	defer func() {
		visible := []model.Run{result}
		result = visible[0]
	}()
	if run.SupersededBy == "" || service.history == nil {
		return service.presentationRunWithSuccessor(ctx, run, nil)
	}
	successor, err := service.history.Run(ctx, run.SupersededBy)
	if err != nil {
		return service.presentationRunWithSuccessor(ctx, run, nil)
	}
	return service.presentationRunWithSuccessor(ctx, run, map[string]model.Run{successor.ID: successor})
}

// presentationRuns resolves every successor in one lookup for list views.
func (service *API) presentationRuns(ctx context.Context, runs []model.Run) (result []model.Run, resultErr error) {
	defer func() {
		for i := range result {
			result[i].ResourceMessage = ""
		}
	}()
	batch, ok := service.history.(interface {
		RunsByIDs(context.Context, []string) (map[string]model.Run, error)
	})
	if !ok {
		plain := *service
		for i := range runs {
			runs[i] = plain.presentationRun(ctx, runs[i])
		}
		return runs, nil
	}
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, run := range runs {
		if run.SupersededBy != "" && !seen[run.SupersededBy] {
			seen[run.SupersededBy] = true
			ids = append(ids, run.SupersededBy)
		}
	}
	successors := map[string]model.Run{}
	if len(ids) > 0 {
		var err error
		successors, err = batch.RunsByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
	}
	for i := range runs {
		runs[i] = service.presentationRunWithSuccessor(ctx, runs[i], successors)
	}
	return runs, nil
}

func (service *API) presentationRunWithSuccessor(ctx context.Context, run model.Run, successors map[string]model.Run) model.Run {
	visible := presentationRunAt(run, service.schedulingClock().Now())
	successor, ok := successors[run.SupersededBy]
	if !ok {
		if service.scope != nil && run.SupersededBy != "" {
			visible.SupersededBy = ""
		}
		return visible
	}
	if run.Status == model.RunInterrupted && run.Phase != model.PhaseCancelled && run.Reason == "superseded by newer run" {
		sha := successor.SHA
		if len(sha) > 8 {
			sha = sha[:8]
		}
		visible.DisplayReason = "superseded by " + sha
	}
	return visible
}

// burnPrecedence resolves a burn to one word from the states of its steps.
// Highest wins, so the answer does not depend on the order the steps arrive
// in: any failure makes the burn failed, otherwise the least-advanced step
// that still has work ahead of it names the burn, and only a burn whose steps
// have all finished reports a finished state.
//
// pending sits between queued and passed for exactly that reason. A burn with
// one passed step and one the run never reached is not a passed burn, and
// before the plan existed it could not say so -- the unreached step was absent
// from the list entirely, so the burn read "passed".
var stagePrecedence = map[string]int{"-": 0, "passed": 1, "pending": 2, "queued": 3, "running": 4, "failed": 5}

func wireStages(steps []model.StepResult, run model.Run) (map[string]string, *int) {
	stages := make(map[string]string)
	var exitCode *int
	if canonical := runlog.LegacyRetryStage(run.FailedStage); canonical != run.FailedStage {
		// A legacy result may now resolve a declaration under its canonical
		// name. Match that failure identity too, while preserving literal
		// suffixed burns that still have their own row.
		literal := false
		for _, step := range steps {
			if step.Stage == run.FailedStage {
				literal = true
				break
			}
		}
		if !literal {
			run.FailedStage = canonical
		}
	}
	for _, step := range steps {
		state := ""
		switch step.Status {
		case model.StepFailed, model.StepTimedOut:
			state = "failed"
			if exitCode == nil && matchesFailedStep(step, run) {
				value := step.ExitCode
				exitCode = &value
			}
		case model.StepRunning:
			state = "running"
		case model.StepQueued:
			state = "queued"
		case model.StepPending:
			state = "pending"
		case model.StepPassed:
			state = "passed"
		case model.StepSkipped:
			// On terminal runs this also includes declarations never reached
			// before an engine error or cancellation, beyond Argo's guards.
			state = "-"
		default:
			continue
		}
		if current, seen := stages[step.Stage]; !seen || stagePrecedence[state] > stagePrecedence[current] {
			stages[step.Stage] = state
		}
	}
	return stages, exitCode
}

// matchesFailedStep returns true when the given step result matches the run's
// recorded failed step identity. When both FailedBurn and FailedStep are
// populated, both must match; when only one is populated, it alone decides.
// When neither is populated, the first failed step wins (the caller already
// checks exitCode == nil).
func matchesFailedStep(step model.StepResult, run model.Run) bool {
	stageRecorded := run.FailedStage != ""
	stepRecorded := run.FailedStep != ""
	if !stageRecorded && !stepRecorded {
		return true // no identity recorded; take the first failed step
	}
	stageMatch := !stageRecorded || step.Stage == run.FailedStage
	stepMatch := !stepRecorded || step.Step == run.FailedStep
	return stageMatch && stepMatch
}

// stepsForRun assembles the run's step list from the three records that each
// know a different amount about it: the plan knows which steps exist, the
// progress journal knows which ones started, and the durable results know how
// they ended.
//
// All three are read for terminal runs too, not just active ones. A run that
// was interrupted mid-flight records no results at all -- persistSteps only
// ever runs from a completed Job's result -- so reading only the store gave it
// an empty step list, discarding both the plan it was admitted with and the
// progress it had visibly made. Its honest record is the plan, with the steps
// that reached a state filled in and unreached declarations marked skipped.
func (service *API) stepsForRun(ctx context.Context, run model.Run) (result []model.StepResult, resultErr error) {

	steps, err := service.history.StepResults(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if service.logs == nil {
		return steps, nil
	}
	planned, err := service.logs.StepPlan(run.ID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A run submitted before plans existed, or by an engine that cannot
		// enumerate its document, simply has none.
		planned = nil
	}
	events, err := service.logs.StepProgress(run.ID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		events = nil
	case err != nil && run.Status.Active():
		// While a run is live the journal is the primary record of in-flight
		// state, so failing to read it is a real failure.
		return nil, fmt.Errorf("load active step progress: %w", err)
	case err != nil:
		// For a terminal run the journal only supplements durable results.
		// Degrading to those beats failing the request for a finished run.
		events = nil
	}
	if len(planned) == 0 && len(events) == 0 {
		return steps, nil
	}
	return mergeSteps(run, planned, events, steps), nil
}

// mergeSteps overlays the plan, progress journal, and durable results in
// increasing order of authority. A source advances a step's state; legacy
// retry aliases first select the highest attempt, which may restart that
// state. Declared rows retain their planned names and ordinals throughout.
func mergeSteps(run model.Run, planned []runprogress.PlannedStep, events []runprogress.Event, persisted []model.StepResult) []model.StepResult {
	order := make(map[string]int, len(planned))
	secrets := make(map[string]bool, len(planned))
	for _, step := range planned {
		order[step.Stage+"\x00"+step.Step] = step.Ordinal
		secrets[step.Stage+"\x00"+step.Step] = step.Secrets
	}
	canonicalObserved := make(map[string]bool)
	for _, event := range events {
		canonicalObserved[event.Stage+"\x00"+event.Step] = true
	}
	for _, result := range persisted {
		canonicalObserved[result.Stage+"\x00"+result.Step] = true
	}
	merged := make([]model.StepResult, 0, len(planned)+len(events)+len(persisted))
	position := make(map[string]int, cap(merged))
	latestLegacy := make(map[string]int)
	advance := func(candidate model.StepResult) {
		key := candidate.Stage + "\x00" + candidate.Step
		newAttempt := false
		if _, isPlanned := order[key]; !isPlanned {
			canonical := runlog.LegacyRetryStage(candidate.Stage)
			canonicalKey := canonical + "\x00" + candidate.Step
			if _, planned := order[canonicalKey]; planned && canonical != candidate.Stage {
				// A modern canonical observation supersedes the legacy alias,
				// as it does when the journal is restored for adoption.
				if canonicalObserved[canonicalKey] {
					return
				}
				attempt, err := strconv.Atoi(candidate.Stage[len(canonical)+1 : len(candidate.Stage)-1])
				if err == nil {
					// Old durable results used the attempt as their burn. Select
					// by attempt number, not result order or timestamps, and keep
					// the declaration's ordinal and the attempt's other evidence.
					previous, seen := latestLegacy[canonicalKey]
					if seen && attempt < previous {
						return
					}
					newAttempt = !seen || attempt > previous
					latestLegacy[canonicalKey] = attempt
					candidate.Stage, key = canonical, canonicalKey
				}
			}
		}
		if ordinal, isPlanned := order[key]; isPlanned {
			// Only the plan knows whether the step runs a credential chain;
			// observations and durable results never carry it.
			candidate.Ordinal, candidate.Secrets = ordinal, secrets[key]
		} else if len(order) != 0 {
			// Observed but never planned. It sorts after every planned step
			// rather than displacing one, keeping the plan's own order intact.
			candidate.Ordinal += len(order)
		}
		index, seen := position[key]
		if !seen {
			position[key] = len(merged)
			merged = append(merged, candidate)
			return
		}
		if newAttempt || candidate.Status.Progress() >= merged[index].Status.Progress() {
			merged[index] = candidate
		}
	}
	for _, step := range planned {
		advance(model.StepResult{
			RunID: run.ID, Stage: step.Stage, Step: step.Step,
			Ordinal: step.Ordinal, Status: model.StepPending, Secrets: step.Secrets,
		})
	}
	for _, event := range events {
		advance(progressStepResult(run.ID, event))
	}
	for _, result := range persisted {
		advance(result)
	}
	if run.Status.Terminal() {
		for index := range merged {
			// Resolve only untouched plan rows, after applying real outcomes.
			// A terminal run cannot reach these steps later (for example a
			// false when-condition or an untaken DAG branch). Keep the plan's
			// ordinal and leave execution timestamps and log ranges absent.
			if merged[index].Status == model.StepPending {
				merged[index].Status = model.StepSkipped
			}
		}
	}
	sort.SliceStable(merged, func(left, right int) bool {
		if merged[left].Ordinal != merged[right].Ordinal {
			return merged[left].Ordinal < merged[right].Ordinal
		}
		if merged[left].Stage != merged[right].Stage {
			return merged[left].Stage < merged[right].Stage
		}
		return merged[left].Step < merged[right].Step
	})
	return merged
}

func progressStepResult(runID string, event runprogress.Event) model.StepResult {
	return model.StepResult{
		RunID: runID, Stage: event.Stage, Step: event.Step, Ordinal: event.Ordinal,
		Status: model.StepStatus(event.Status), ExitCode: event.ExitCode,
		StartedAt: event.StartedAt, FinishedAt: event.FinishedAt,
	}
}

func toolLogFilter(pattern string, context, offset, limit int, tail bool) (runlog.Filter, error) {
	for _, field := range []struct {
		name  string
		value int
	}{{"context", context}, {"offset", offset}, {"limit", limit}} {
		if field.value < 0 {
			return runlog.Filter{}, fmt.Errorf("%w: %s must be a non-negative line count", ErrInvalidInput, field.name)
		}
	}
	return runlog.Filter{Pattern: pattern, Context: context, Offset: offset, Limit: limit, Tail: tail}, nil
}

func (service *API) logsFor(ctx context.Context, repositoryName, selector, wanted string, filter runlog.Filter) (LogResponse, error) {
	if service.logs == nil || service.history == nil {
		return LogResponse{}, refuse(ErrUnavailable, "logs")
	}
	if !validSHASelector(selector) || strings.TrimSpace(wanted) == "" {
		return LogResponse{}, fmt.Errorf("%w: SHA and step are required", ErrInvalidInput)
	}
	_, run, err := service.resolveRun(ctx, repositoryName, selector)
	if err != nil {
		return LogResponse{}, err
	}
	steps, err := service.stepsForRun(ctx, run)
	if err != nil {
		return LogResponse{}, fmt.Errorf("load run steps: %w", err)
	}
	stage, step, err := selectLogRange(steps, wanted)
	if err != nil {
		return LogResponse{}, err
	}
	return service.stepLogResponse(ctx, run, steps, stage, step, filter)
}

func (service *API) stepLogResponse(ctx context.Context, run model.Run, steps []model.StepResult, stage, step string, filter runlog.Filter) (LogResponse, error) {
	if run.Status.Terminal() {
		forgetActiveLogIndex(service.logs, run.ID)
	}
	for _, result := range steps {
		if result.Stage != stage || result.Step != step {
			continue
		}
		// A workflow can fail before this step ever gets a pod or writes a
		// log marker. Its durable diagnostic explains the absent file/range;
		// do not turn that expected absence into a storage failure.
		if run.Status.Terminal() && result.LogStart == 0 && result.LogEnd == 0 &&
			result.Termination != nil && !result.Termination.Available && result.Termination.Unavailable == "not-executed" {
			if err := service.purgedEvidence(ctx, run, "logs"); err != nil {
				return LogResponse{}, err
			}
			body, meta, err := runlog.FilterBytes(nil, filter)
			if err != nil {
				return LogResponse{}, err
			}
			response := newLogResponse(run.ID, stage, step, body, meta)
			response.Note = "step was not executed; no log recorded"
			response.RunError = run.Error
			return response, nil
		}
		break
	}
	body, meta, err := service.readStepLogFiltered(ctx, run, stage, step, filter)
	if err != nil {
		return LogResponse{}, fmt.Errorf("read bounded run log: %w", err)
	}
	return newLogResponse(run.ID, stage, step, body, meta), nil
}

func newLogResponse(runID, stage, step string, body []byte, meta runlog.Meta) LogResponse {
	if meta.ScanLimitBytes == 0 {
		meta.ScanLimitBytes = runlog.MaxScanBytes
	}
	return LogResponse{
		RunID: runID, Stage: stage, Step: step, Output: string(body),
		TotalLines: meta.TotalLines, MatchedLines: meta.MatchedLines,
		ReturnedLines: meta.ReturnedLines, Truncated: meta.Truncated, Bytes: meta.Bytes,
		ScanComplete: meta.ScanComplete, BytesExact: meta.BytesExact, ScannedBytes: meta.ScannedBytes, DiscoveryBytes: meta.DiscoveryBytes,
		TotalScannedBytes: meta.ScannedBytes + meta.DiscoveryBytes, ScanLimitBytes: meta.ScanLimitBytes,
		WithheldBytes: meta.WithheldBytes, WithheldLines: meta.WithheldLines, OmittedLongLines: meta.OmittedLongLines,
		HasMore: meta.HasMore, NextByteOffset: meta.NextByteOffset,
	}
}

func (service *API) readStepLogFiltered(ctx context.Context, run model.Run, stage, step string, filter runlog.Filter) ([]byte, runlog.Meta, error) {
	filter = filter.WithContext(ctx)
	if err := service.purgedEvidence(ctx, run, "logs"); err != nil {
		return nil, runlog.Meta{}, err
	}
	if filter.Pattern != "" {
		release, err := runLogPatternScans.acquire(ctx)
		if err != nil {
			return nil, runlog.Meta{}, err
		}
		defer release()
	}
	if run.Status.Active() {
		body, meta, err := service.logs.ReadActiveFiltered(run.ID, stage, step, filter)
		if errors.Is(err, os.ErrNotExist) {
			// Argo may journal progress before the first pod output arrives.
			// Only an already-authorized step reaches this retained-log read;
			// a missing file or range while active is pending, not a fault.
			return nil, runlog.Meta{}, fmt.Errorf("%w: run %s is %s; retained output for burn %q step %q is not available yet; retry while the step runs", ErrStepLogPending, run.ID, run.Status, stage, step)
		}
		return body, meta, err
	}
	return service.logs.ReadFiltered(run.ID, stage, step, filter)
}

func selectLogRange(results []model.StepResult, wanted string) (string, string, error) {
	selectedStage := ""
	for _, result := range results {
		if result.Step != wanted {
			continue
		}
		if selectedStage != "" {
			return "", "", fmt.Errorf("%w: step %q is ambiguous across burns", ErrInvalidInput, wanted)
		}
		selectedStage = result.Stage
	}
	if selectedStage == "" {
		return "", "", fmt.Errorf("no log range for step %q: %w", wanted, store.ErrNotFound)
	}
	return selectedStage, wanted, nil
}

func (service *API) prepareWaitRun(ctx context.Context, repositoryName, selector, trigger string, requestedSeconds int, actor string) (func(context.Context) (WaitResponse, error), error) {
	if !validSHASelector(selector) {
		return nil, fmt.Errorf("%w: full or short SHA is required", ErrInvalidInput)
	}
	trigger = normalizeTrigger(strings.TrimSpace(trigger))
	return service.prepareWaitStatus(ctx, trigger, requestedSeconds, func(ctx context.Context) (StatusResponse, error) {
		return service.waitStatus(ctx, repositoryName, selector, trigger, actor)
	})
}

// prepareWaitRunID pins the unique prefix once, so a later collision or rerun
// cannot change which run this observer waits for.
func (service *API) prepareWaitRunID(ctx context.Context, id string, requestedSeconds int) (func(context.Context) (WaitResponse, error), error) {
	if service.history == nil || service.repositories == nil {
		return nil, refuse(ErrUnavailable, "run history")
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return nil, err
	}
	return service.prepareWaitStatus(ctx, "", requestedSeconds, func(ctx context.Context) (StatusResponse, error) {
		current, err := service.history.Run(ctx, run.ID)
		if err != nil {
			return StatusResponse{}, err
		}
		repository, err := service.repositories.Repository(ctx, current.RepoID)
		if err != nil {
			return StatusResponse{}, err
		}
		return service.statusForRun(ctx, repository, current)
	})
}

func (service *API) prepareWaitStatus(ctx context.Context, trigger string, requestedSeconds int, observe func(context.Context) (StatusResponse, error)) (func(context.Context) (WaitResponse, error), error) {
	duration, err := service.waitDuration(requestedSeconds)
	if err != nil {
		return nil, err
	}
	clock := service.schedulingClock()
	deadline := clock.Now().Add(duration)
	status, err := observe(ctx)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (WaitResponse, error) {
		timer := clock.NewTimer(deadline.Sub(clock.Now()))
		defer timer.Stop()
		for {
			// If no matching trigger exists yet, waitStatus preserves the current
			// unfiltered status. It must not satisfy the awaited trigger.
			triggerMatch := trigger == "" || status.Run.Trigger == trigger
			if triggerMatch && status.Run.Status.Terminal() {
				return WaitResponse{StatusResponse: status}, nil
			}
			changed := service.signals.Run(status.Run.ID)
			// Re-read after subscribing so a transition between the first read and
			// channel lookup cannot be lost.
			status, err = observe(ctx)
			if err != nil {
				return WaitResponse{}, err
			}
			triggerMatch = trigger == "" || status.Run.Trigger == trigger
			if triggerMatch && status.Run.Status.Terminal() {
				return WaitResponse{StatusResponse: status}, nil
			}
			select {
			case <-ctx.Done():
				return WaitResponse{}, ctx.Err()
			case <-timer.C():
				// When a trigger filter is active and the resolved run does not
				// match it, the awaited run was never seen. Report still_running
				// so a caller that checks only the flag does not mistake a
				// terminal branch run for the awaited release run (#439).
				stillRunning := !status.Run.Status.Terminal()
				if !triggerMatch {
					stillRunning = true
				}
				if status.Scheduling != nil {
					status.Scheduling.AgeSeconds = max(0, int64(clock.Since(status.Scheduling.ObservedAt)/time.Second))
				}
				return WaitResponse{StatusResponse: status, StillRunning: stillRunning}, nil
			case <-changed:
				status, err = observe(ctx)
				if err != nil {
					return WaitResponse{}, err
				}
			}
		}
	}, nil
}

func (service *API) waitStatus(ctx context.Context, repositoryName, selector, trigger, actor string) (StatusResponse, error) {
	resolve := func(ctx context.Context, repoID int64, selector string) (model.Run, error) {
		return service.runs.ResolveWaitRun(ctx, repoID, selector, trigger)
	}
	repository, run, err := service.resolveRunWith(ctx, repositoryName, selector, resolve)
	if errors.Is(err, store.ErrNotFound) {
		// Keep the established timeout response while a release has not yet
		// been admitted. A terminal branch never completes a release wait.
		return service.status(ctx, repositoryName, selector, actor)
	}
	if err != nil {
		return StatusResponse{}, err
	}
	return service.statusForRun(ctx, repository, run)
}

// normalizeTrigger maps the release trigger to its persisted tag form.
func normalizeTrigger(trigger string) string {
	switch trigger {
	case "release":
		return "tag"
	default:
		return trigger
	}
}

func validSHASelector(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, character := range strings.ToLower(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (service *API) sync(ctx context.Context, actor api.Actor, repositoryName, selector, branch string) (model.Run, error) {
	if service.git == nil || service.auditor == nil {
		return model.Run{}, refuse(ErrUnavailable, "sync")
	}
	if !validOID(selector) {
		return model.Run{}, fmt.Errorf("%w: sync requires a full SHA", ErrInvalidInput)
	}
	repository, run, err := service.resolveRun(ctx, repositoryName, selector)
	if err != nil {
		return model.Run{}, err
	}
	if repository.ArchivedAt != nil {
		return model.Run{}, store.ErrRepositoryArchived
	}
	if run.RefKind != model.RefBranch {
		return model.Run{}, fmt.Errorf("%w: sync only accepts branch runs", ErrInvalidInput)
	}
	// Exclude promotion, release, and internal-trigger runs before any audit
	// or Git mutation. A commit shared across branches can resolve to a
	// promotion run with a synthetic RefBranch, which would rewind the wrong
	// real branch.
	trigger := strings.TrimSpace(run.Trigger)
	if trigger == "promotion" || trigger == "release" || trigger == "apply" || trigger == "plan" {
		return model.Run{}, fmt.Errorf("%w: sync does not accept %s runs", ErrInvalidInput, trigger)
	}
	// When an explicit branch is provided, validate the resolved run matches
	// it exactly. If the initial SHA-based resolution returned a run on a
	// different branch (because resolveRun picks the highest-queue-sequence
	// run), try ref-based resolution for the requested branch and verify
	// the SHA matches.
	if trimmedBranch := strings.TrimSpace(branch); trimmedBranch != "" {
		if run.Ref != trimmedBranch {
			// The SHA-resolved run is on a different branch. Try to find a
			// run on the requested branch whose SHA matches.
			branchRun, branchErr := service.runs.ResolveRun(ctx, repository.ID, trimmedBranch)
			if branchErr != nil || branchRun.SHA != run.SHA {
				return model.Run{}, fmt.Errorf("%w: no run for SHA %s on branch %q", ErrInvalidInput, selector, trimmedBranch)
			}
			run = branchRun
			// Re-check trigger exclusion for the branch-resolved run.
			branchTrigger := strings.TrimSpace(run.Trigger)
			if branchTrigger == "promotion" || branchTrigger == "release" || branchTrigger == "apply" || branchTrigger == "plan" {
				return model.Run{}, fmt.Errorf("%w: sync does not accept %s runs", ErrInvalidInput, branchTrigger)
			}
		}
	} else if resolver, ok := service.runs.(BranchRefResolver); ok {
		// No explicit branch — verify the SHA is not ambiguous across
		// branches. A bare-SHA sync of a commit with branch-trigger runs on
		// multiple distinct branches would silently resolve to the
		// highest-queue-sequence run and could force-push a branch the
		// caller did not intend.
		candidateRefs, refErr := resolver.DistinctBranchRefsForSHA(ctx, repository.ID, run.SHA)
		if refErr != nil {
			return model.Run{}, fmt.Errorf("check branch ambiguity: %w", refErr)
		}
		if len(candidateRefs) > 1 {
			return model.Run{}, fmt.Errorf("%w: SHA %s has branch-trigger runs on multiple branches (%s); pass the explicit branch argument to disambiguate",
				ErrInvalidInput, run.SHA, strings.Join(candidateRefs, ", "))
		}
	}
	// Refuse to sync the default branch — that is promote's job. Syncing
	// the default branch would bypass the green gate and silently rewind or
	// force-push main, which is never correct.
	if run.Ref == repository.DefaultBranch {
		return model.Run{}, fmt.Errorf("%w: sync refuses the default branch %q — use promote to integrate",
			ErrInvalidInput, repository.DefaultBranch)
	}

	if err := auditedGitMutation(ctx, service.auditor, actor.Identity, "sync.branch", "run", run.ID,
		map[string]any{"repo": repository.Name, "branch": run.Ref, "sha": run.SHA},
		func() error {
			return service.git.SyncBranch(ctx, service.cacheInputFor(ctx, repository), run.Ref, run.SHA)
		}); err != nil {
		return model.Run{}, fmt.Errorf("sync branch: %w", err)
	}
	return run, nil
}

func (service *API) promote(ctx context.Context, actor api.Actor, repositoryName, selector, target string) (result model.Promotion, resultErr error) {
	// Without either grant or repository-policy capability, only admins
	// can request promotion.
	if !actor.Admin && service.promotionGrants == nil {
		_, policyOK := service.git.(promotionPolicyReader)
		_, parkOK := service.git.(parkSourceResolver)
		if !policyOK || !parkOK {
			return model.Promotion{}, refuse(ErrForbidden, "promote requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub")
		}
	}
	if service.git == nil || service.promotions == nil || service.promotionRuns == nil || service.enqueues == nil || service.auditor == nil {
		return model.Promotion{}, refuse(ErrUnavailable, "promotion")
	}
	if strings.TrimSpace(target) == "" {
		return model.Promotion{}, refuse(ErrInvalidInput, "promotion target is required")
	}
	if err := validatePromotionTarget(target); err != nil {
		return model.Promotion{}, err
	}
	if !validOID(selector) {
		return model.Promotion{}, refuse(ErrInvalidInput, "promotion requires a full SHA")
	}
	repository, candidate, err := service.resolvePromotionSource(ctx, repositoryName, selector)
	if err != nil {
		return model.Promotion{}, err
	}
	promotion, err := service.admitPromotion(ctx, actor, repository, candidate, target, "", "")
	if err != nil {
		return model.Promotion{}, err
	}
	return service.planOrQueuePromotion(ctx, actor, repository, candidate, target, promotion)
}

// validatePromotionTarget accepts a branch name only. A full ref was admitted
// and audited before, then failed when the merge was prepared, and again on
// every retry.
func validatePromotionTarget(target string) error {
	if strings.HasPrefix(target, "refs/") {
		return refuse(ErrInvalidInput, "promotion target %q is a ref; name the target branch, e.g. %q", target, strings.TrimPrefix(target, "refs/heads/"))
	}
	if err := gitcache.ValidateBranch(target); err != nil {
		return refuse(ErrInvalidInput, "promotion target: %v", err)
	}
	return nil
}

// admitPromotion authorizes and durably admits a green branch candidate or
// an authorized parked source. Admins act directly; trusted repository opt-in
// also authorizes a non-admin parked source. Otherwise non-admins need a
// matching unexpired, unrevoked, unconsumed grant for the exact
// (repository, SHA, target) that no pending promotion holds. The promotion
// holds the grant until it terminalizes; the grant is consumed only with its
// delivered publication (#1097), so a transient failure before delivery does
// not spend it. retryOf and mode record a promote_retry lineage.
func (service *API) admitPromotion(ctx context.Context, actor api.Actor, repository model.Repository, candidate model.Run,
	target, retryOf, mode string) (model.Promotion, error) {
	skipped, policyOptIn, err := service.checkPromotionSource(ctx, repository, candidate, target)
	if err != nil {
		return model.Promotion{}, err
	}
	grantID, err := service.promotionSourceGrant(ctx, actor, repository, candidate, target, retryOf, mode, 0, policyOptIn)
	if err != nil {
		return model.Promotion{}, err
	}
	promotion, err := service.promotions.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repository.ID, SourceBranch: candidate.Ref, SourceSHA: candidate.SHA,
		TargetRef: target, Actor: actor.Identity, GrantID: grantID,
		RetryOf: retryOf, RetryMode: mode, BranchCISkipped: skipped,
	})
	if err != nil {
		return model.Promotion{}, fmt.Errorf("admit promotion: %w", err)
	}
	return promotion, nil
}

// planAdmittedPromotion prepares an admitted promotion against the target's
// current head, then publishes a fast-forward or enqueues the merged run.
func (service *API) planAdmittedPromotion(ctx context.Context, actor api.Actor, repository model.Repository, candidate model.Run,
	target string, promotion model.Promotion) (result model.Promotion, resultErr error) {
	if promotion.BranchCISkipped {
		policy, err := readPromotionPolicy(ctx, service.git, service.cacheInputFor(ctx, repository))
		if err != nil {
			return service.failAdmittedPromotion(ctx, promotion, err.Error(), "")
		}
		if policy.requiresBranchCI(target) {
			return service.failAdmittedPromotion(ctx, promotion, "target policy requires branch CI", "")
		}
	}
	if err := os.MkdirAll(service.promotionWorkspaceRoot, 0o750); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "create promotion workspace root: "+err.Error(), "")
	}
	workspaceParent := filepath.Join(service.promotionWorkspaceRoot, "promotion-"+promotion.ID)
	workspaceSource := filepath.Join(workspaceParent, "src")
	if !pathWithin(service.promotionWorkspaceRoot, workspaceParent) || !pathWithin(workspaceParent, workspaceSource) {
		return service.failAdmittedPromotion(ctx, promotion, "promotion workspace escaped its configured root", "")
	}
	if err := os.Mkdir(workspaceParent, 0o700); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "reserve promotion workspace: "+err.Error(), "")
	}
	defer func() {
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		defer cancelCleanup()
		result = service.cleanupTerminalPromotionWorkspace(cleanupCtx, result)
	}()
	var plan gitcache.MergeCandidate
	var err error
	var resolvedTrain *model.PromotionTrain
	if promotion.Train != nil {
		if _, err = service.resolvePromotionTrainSources(ctx, repository, promotion.Train, target); err == nil {
			planner, ok := service.git.(promotionTrainGit)
			if !ok {
				err = refuse(ErrUnavailable, "promotion train preparation")
			} else {
				var prepared gitcache.PromotionTrainCandidate
				prepared, err = planner.PreparePromotionTrain(ctx, service.cacheInputFor(ctx, repository), promotion.Train.Sources, target, workspaceSource, promotion.Train.SkipConflicting)
				plan = prepared.MergeCandidate
				resolvedTrain = &model.PromotionTrain{Sources: prepared.Sources, SkipConflicting: promotion.Train.SkipConflicting, TreeSHA: prepared.TreeSHA}
				if err != nil && len(prepared.Sources) == len(promotion.Train.Sources) {
					resolvedTrain.TreeSHA = ""
					if recorder, ok := service.promotions.(promotionTrainPlanner); ok {
						if recorded, recordErr := recorder.PlanPromotionTrain(ctx, promotion.ID, "", "", resolvedTrain); recordErr != nil {
							err = errors.Join(err, recordErr)
						} else {
							promotion = recorded
						}
					}
				}
			}
		}
	} else {
		var reused bool
		plan, reused, err = service.reusablePromotionPlan(ctx, repository, promotion)
		if err == nil && !reused {
			plan, err = service.git.PreparePromotion(ctx, service.cacheInputFor(ctx, repository), candidate.SHA, target, workspaceSource)
		}
	}
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "prepare promotion: "+err.Error(), "")
	}
	if !validOID(plan.MergedSHA) ||
		(plan.TargetUnborn && (plan.BaseSHA != "" || promotion.Train == nil && !plan.FastForward)) ||
		(!plan.TargetUnborn && !validOID(plan.BaseSHA)) {
		return service.failAdmittedPromotion(ctx, promotion, "promotion plan contains invalid object IDs", "")
	}
	// An unborn target (brand-new repository, issue #264) has no base
	// commit. The promotion row records Git's canonical zero OID — the
	// receive-pack representation of a created ref — because the store
	// schema requires a concrete planned base; the publication keeps the
	// empty previous, which is the delivery layer's creation contract.
	planBase := plan.BaseSHA
	if plan.TargetUnborn {
		planBase = zeroOID
	}
	if err := service.checkPromotionRelease(ctx, repository, plan.MergedSHA, plan.BaseSHA); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, err.Error(), "")
	}
	var planned model.Promotion
	if promotion.Train != nil {
		recorder, ok := service.promotions.(promotionTrainPlanner)
		if !ok {
			return service.failAdmittedPromotion(ctx, promotion, "promotion train records unavailable", "")
		}
		if plan.FastForward {
			return service.failAdmittedPromotion(ctx, promotion, "promotion train requires merged CI", "")
		}
		planned, err = recorder.PlanPromotionTrain(ctx, promotion.ID, planBase, plan.MergedSHA, resolvedTrain)
	} else {
		planned, err = service.promotions.PlanPromotion(ctx, promotion.ID, planBase, plan.MergedSHA)
	}
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "record promotion plan: "+err.Error(), "")
	}
	promotion = planned
	// The promotion.pending audit action is now recorded atomically inside
	// Store.PlanPromotion's own transaction — committed before PlanPromotion
	// returns and before any enqueue, so no competing queue wake can claim
	// unaudited work. A separate service-level auditPromotion call here
	// would duplicate the action.
	if plan.FastForward && !promotion.BranchCISkipped {
		publication, err := service.promotions.BeginPublication(ctx, model.PublicationSpec{
			RepoID: repository.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: target,
			PreviousSHA: planBase, PreviousKnown: true,
			ResultSHA: plan.MergedSHA, Actor: actor.Identity,
		})
		if err != nil {
			return service.failAdmittedPromotion(ctx, promotion, "record fast-forward publication: "+err.Error(), "")
		}
		deliveryCtx, cancelDelivery := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
		finalization, err := service.enqueues.DeliverPublication(deliveryCtx, publication.ID)
		cancelDelivery()
		if err != nil {
			service.enqueues.NotifyPublicationRecovery(publication.ID)
			lookupCtx, cancelLookup := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
			current, lookupErr := service.promotions.Promotion(lookupCtx, promotion.ID)
			cancelLookup()
			if lookupErr == nil {
				current.Error = fmt.Sprintf("publication %s awaits recovery: %v", publication.ID, err)
				return current, nil
			}
			promotion.Error = errors.Join(err, lookupErr).Error()
			return promotion, nil
		}
		service.enqueues.NotifyQueue()
		service.signals.NotifyPromotion(promotion.ID)
		return finalization.Promotion, nil
	}

	enqueued, attached, err := service.promotionRuns.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{

		RepoID: repository.ID, RefKind: model.RefBranch,
		Ref: promotionRunRef(target, plan.MergedSHA), SHA: plan.MergedSHA,
		Actor: actor.Identity, Trigger: "promotion", TestedSHA: plan.MergedSHA, BaseSHA: plan.BaseSHA,
	}, promotion.ID)
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "record merged promotion run: "+err.Error(), "")
	}
	promotion = attached
	if err := service.enqueues.AcceptEnqueue(ctx, enqueued); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, err.Error(), enqueued.ID)
	}
	return promotion, nil
}

func (service *API) failAdmittedPromotion(ctx context.Context, promotion model.Promotion, reason, runID string) (model.Promotion, error) {
	// Admission is already durable. Compensation must outlive a disconnected or
	// timed-out caller, but it must still have a finite storage deadline.
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
	defer cancel()
	if runID != "" {
		_, err := service.promotionRuns.FinishRun(finalizeCtx, runID, model.RunResult{
			Status: model.RunFailed, Phase: "promotion", Error: reason,
		})
		service.signals.NotifyRun(runID)
		service.signals.NotifyPromotion(promotion.ID)
		if err != nil {
			promotion.Error = errors.Join(errors.New(reason), err).Error()
			return promotion, nil
		}
		failed, err := service.promotions.Promotion(finalizeCtx, promotion.ID)
		if err != nil {
			promotion.Error = errors.Join(errors.New(reason), err).Error()
			return promotion, nil
		}
		return failed, nil
	}
	failed, err := service.promotions.FinishPromotion(finalizeCtx, promotion.ID, model.PromotionFailed, "", reason)
	service.signals.NotifyPromotion(promotion.ID)
	if err != nil {
		promotion.Error = errors.Join(errors.New(reason), err).Error()
		return promotion, nil
	}
	return failed, nil
}

func promotionRunRef(target, sha string) string {
	suffix := sha
	if len(sha) >= 12 {
		suffix = sha[:12]
	} else if sha == "" {
		suffix = "unknown"
	}
	return "promotion/" + strings.Trim(target, "/") + "/" + suffix
}

func (service *API) prepareWaitPromotion(ctx context.Context, id string, requestedSeconds int) (func(context.Context) (PromotionWaitResponse, error), error) {
	if service.promotions == nil {
		return nil, refuse(ErrUnavailable, "promotions")
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: promotion ID is required", ErrInvalidInput)
	}
	duration, err := service.waitDuration(requestedSeconds)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(duration)
	promotion, err := service.resolveReadPromotionID(ctx, id)
	if err != nil {
		return nil, err
	}
	id = promotion.ID
	repo, err := service.repositoryIdentity(ctx, promotion.RepoID)
	if err != nil {
		return nil, err
	}
	terminal := func(ctx context.Context, promotion model.Promotion) (PromotionWaitResponse, error) {
		retriedAs, err := service.promotionRetriedAs(ctx, promotion)
		if err != nil {
			return PromotionWaitResponse{}, err
		}
		response := PromotionWaitResponse{SourceCount: promotion.SourceCount(), Promotion: promotion, Repo: repo, Ref: promotion.SourceBranch, RetriedAs: retriedAs}
		if service.agentEvents != nil {
			if request, err := service.agentEvents.store.PromotionPrune(ctx, promotion.ID); err == nil {
				response.PruneSource = &request
			}
			if tag, err := service.agentEvents.store.PromotionTag(ctx, promotion.ID); err == nil {
				response.TagAfter = &tag
			}
		}
		return response, nil
	}
	return func(ctx context.Context) (PromotionWaitResponse, error) {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		for {
			if promotion.Status.Terminal() {
				return terminal(ctx, promotion)
			}
			changed := service.signals.Promotion(id)
			promotion, err = service.promotions.Promotion(ctx, id)
			if err != nil {
				return PromotionWaitResponse{}, err
			}
			if promotion.Status.Terminal() {
				return terminal(ctx, promotion)
			}
			select {
			case <-ctx.Done():
				return PromotionWaitResponse{}, ctx.Err()
			case <-timer.C:
				promotion, err = service.promotions.Promotion(ctx, id)
				if err != nil {
					return PromotionWaitResponse{}, err
				}
				if promotion.Status.Terminal() {
					return terminal(ctx, promotion)
				}
				response := PromotionWaitResponse{SourceCount: promotion.SourceCount(), Promotion: promotion, Repo: repo, Ref: promotion.SourceBranch}
				response.StillRunning = true
				return response, nil
			case <-changed:
				promotion, err = service.promotions.Promotion(ctx, id)
				if err != nil {
					return PromotionWaitResponse{}, err
				}
			}
		}
	}, nil
}

func (service *API) issueCreate(ctx context.Context, actor api.Actor, title, body, upstream string) (model.Issue, error) {
	if service.issues == nil {
		return model.Issue{}, refuse(ErrUnavailable, "issues")
	}
	if strings.TrimSpace(title) == "" {
		return model.Issue{}, fmt.Errorf("%w: issue title is required", ErrInvalidInput)
	}
	spec := model.ManualIssueSpec{Title: title, Body: body}
	// An issue filed under an upstream is visible to uplinks scoped to it. A
	// scoped actor's issue defaults to its one upstream and must otherwise
	// name one in scope (#073).
	upstream = strings.TrimSpace(upstream)
	switch {
	case upstream != "":
		id, err := service.upstreamID(ctx, upstream)
		if err != nil {
			return model.Issue{}, err
		}
		spec.ScopeUpstreamID = id
	case service.scope != nil && len(service.scope.upIDs) == 1:
		spec.ScopeUpstreamID = service.scope.upIDs[0]
	case service.scope != nil:
		return model.Issue{}, fmt.Errorf("%w: upstream is required: name the upstream the issue belongs to", ErrInvalidInput)
	}
	return service.issues.CreateManualIssue(ctx, actor.Identity, spec)
}

// upstreamID resolves an upstream name the caller may see.
func (service *API) upstreamID(ctx context.Context, name string) (int64, error) {
	if service.scope != nil {
		for id, upstream := range service.scope.upstreams {
			if upstream.Name == name {
				return id, nil
			}
		}
		return 0, fmt.Errorf("%w: upstream %q", store.ErrNotFound, name)
	}
	source, ok := service.runs.(ScopeSource)
	if !ok {
		return 0, refuse(ErrUnavailable, "upstream lookup")
	}
	upstreams, err := source.ListUpstreams(ctx)
	if err != nil {
		return 0, err
	}
	for _, upstream := range upstreams {
		if upstream.Name == name {
			return upstream.ID, nil
		}
	}
	return 0, fmt.Errorf("%w: upstream %q", store.ErrNotFound, name)
}

func (service *API) issueGet(ctx context.Context, _ api.Actor, id int64) (model.Issue, error) {
	if service.issues == nil {
		return model.Issue{}, refuse(ErrUnavailable, "issues")
	}
	if id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	issue, err := service.issues.Issue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	issues := []model.Issue{issue}
	if err := service.labelIssueScopes(ctx, issues); err != nil {
		return model.Issue{}, err
	}
	return service.withIssueLock(ctx, service.observeIssue(issues[0]))
}

func (service *API) issueUpdate(ctx context.Context, actor api.Actor, id int64, title, body, upstream *string, expectedBodySHA256 string, expectedRevision *int64, workStatus *model.IssueWorkStatus) (model.Issue, error) {
	if service.issues == nil {
		return model.Issue{}, refuse(ErrUnavailable, "issues")
	}
	if id <= 0 || (title == nil && body == nil && upstream == nil && workStatus == nil) {
		return model.Issue{}, fmt.Errorf("%w: issue ID and title, body, upstream or work_status are required", ErrInvalidInput)
	}
	patch := model.IssuePatch{WorkStatus: workStatus, Title: title, Body: body, ExpectedRevision: expectedRevision, ExpectedBodySHA256: expectedBodySHA256}
	if upstream != nil {
		name := strings.TrimSpace(*upstream)
		if actor.Admin && actor.Scoped() && (name == "" || strings.EqualFold(name, "global") || !actor.SeesUpstream(name)) {
			return model.Issue{}, fmt.Errorf("%w: upstream", store.ErrNotFound)
		}
		var id int64
		if name == "" || strings.EqualFold(name, "global") {
			if !actor.Admin {
				return model.Issue{}, fmt.Errorf("%w: only an admin may make an issue installation-wide", ErrInvalidInput)
			}
		} else {
			var err error
			id, err = service.upstreamID(ctx, name)
			if err != nil {
				return model.Issue{}, err
			}
		}
		patch.ScopeUpstreamID = &id
	}
	issue, err := service.issues.UpdateIssue(ctx, actor.Identity, id, patch)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			return model.Issue{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		return model.Issue{}, err
	}
	issues := []model.Issue{issue}
	if err := service.labelIssueScopes(ctx, issues); err != nil {
		return model.Issue{}, err
	}
	return issues[0], nil
}

func (service *API) issueAppend(ctx context.Context, actor api.Actor, id int64, text string) (model.Issue, error) {
	if service.issues == nil {
		return model.Issue{}, refuse(ErrUnavailable, "issues")
	}
	if id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	if strings.TrimSpace(text) == "" {
		return model.Issue{}, fmt.Errorf("%w: append text is required", ErrInvalidInput)
	}
	issue, err := service.issues.AppendIssueBody(ctx, actor.Identity, id, text)
	if err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (service *API) issueClose(ctx context.Context, actor api.Actor, id int64) (model.Issue, error) {
	if service.issues == nil || id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	issue, err := service.issues.Issue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	if issue.Kind == model.IssueCI {
		issue, err = service.issues.CloseCIIssue(ctx, actor.Identity, issue.ID, "closed manually by "+actor.Identity)
	} else {
		closed := model.IssueClosed
		issue, err = service.issues.UpdateManualIssue(ctx, actor.Identity, id, model.IssuePatch{State: &closed})
	}
	if err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (service *API) issueDelete(ctx context.Context, actor api.Actor, id int64) error {
	if service.issues == nil {
		return refuse(ErrUnavailable, "issues")
	}
	if id <= 0 {
		return fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	return service.issues.DeleteManualIssue(ctx, actor.Identity, id)
}

func (service *API) issueList(ctx context.Context, repositoryName, kind, state, query string, before int64, limit int, workStatus string) (model.IssuePage, error) {
	if service.issues == nil {
		return model.IssuePage{}, refuse(ErrUnavailable, "issues")
	}
	filter := model.IssueListFilter{WorkStatus: model.IssueWorkStatus(workStatus), Kind: model.IssueKind(kind), State: model.IssueState(state), Query: query, BeforeID: before, Limit: limit}
	if repositoryName != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return model.IssuePage{}, err
		}
		filter.RepoID = repository.ID
	}
	if filter.WorkStatus != "" && !filter.WorkStatus.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue work status", ErrInvalidInput)
	}
	if filter.Kind != "" && !filter.Kind.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue kind", ErrInvalidInput)
	}
	if filter.State != "" && !filter.State.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue state", ErrInvalidInput)
	}
	if filter.BeforeID < 0 {
		return model.IssuePage{}, fmt.Errorf("%w: issue cursor", ErrInvalidInput)
	}
	if filter.Limit <= 0 || filter.Limit > maximumIssuePage {
		filter.Limit = maximumIssuePage
	}
	page, err := service.issues.ListIssues(ctx, filter)
	if err != nil {
		return model.IssuePage{}, err
	}
	if err := service.labelIssueScopes(ctx, page.Issues); err != nil {
		return model.IssuePage{}, err
	}
	for i := range page.Issues {
		page.Issues[i], err = service.withIssueLock(ctx, service.observeIssue(page.Issues[i]))
		if err != nil {
			return model.IssuePage{}, err
		}
	}
	return page, nil
}

func (service *API) labelIssueScopes(ctx context.Context, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	names := map[int64]string{}
	repos := map[int64]int64{}
	if service.scope != nil {
		for id, upstream := range service.scope.upstreams {
			names[id] = upstream.Name
		}
		for id, repo := range service.scope.repos {
			repos[id] = repo.UpstreamID
		}
	} else if source, ok := service.runs.(ScopeSource); ok {
		upstreams, err := source.ListUpstreams(ctx)
		if err != nil {
			return err
		}
		for _, upstream := range upstreams {
			names[upstream.ID] = upstream.Name
		}
		all, err := source.ListRepositories(ctx)
		if err != nil {
			return err
		}
		for _, repo := range all {
			repos[repo.ID] = repo.UpstreamID
		}
	}
	for i := range issues {
		id := issues[i].ScopeUpstreamID
		if issues[i].RepoID != 0 {
			id = repos[issues[i].RepoID]
		}
		issues[i].Upstream = names[id]
	}
	return nil
}

func wireIssue(issue model.Issue) api.IssueResponse {
	return api.IssueResponse{
		Lock: issue.Lock, Revision: issue.Revision,
		WorkStatus: string(issue.WorkStatus), Stale: issue.Stale,
		ID: issue.ID, Kind: string(issue.Kind), Title: issue.Title,
		Body: issue.Body, State: string(issue.State),
		Upstream: issue.Upstream,
		RepoID:   issue.RepoID, Branch: issue.Branch,
		Occurrences: int(issue.Occurrences),
		CIOrigin:    issue.CIOrigin, CIWorkID: issue.CIWorkID,
		CreatedAt: issue.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: issue.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func wireIssueList(page model.IssuePage) api.IssueListResponse {
	issues := make([]api.IssueListItem, len(page.Issues))
	for index, issue := range page.Issues {
		item := api.IssueListItem{
			WorkStatus: string(issue.WorkStatus), Stale: issue.Stale,
			Revision:    issue.Revision,
			Lock:        issue.Lock,
			ID:          issue.ID,
			State:       string(issue.State),
			Kind:        string(issue.Kind),
			Title:       issue.Title,
			Upstream:    issue.Upstream,
			RepoID:      issue.RepoID,
			Branch:      issue.Branch,
			Occurrences: int(issue.Occurrences),
			CIOrigin:    issue.CIOrigin,
			CIWorkID:    issue.CIWorkID,
			CreatedAt:   issue.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:   issue.UpdatedAt.UTC().Format(time.RFC3339),
		}
		issues[index] = item
	}
	return api.IssueListResponse{Issues: issues, NextBefore: page.NextBefore, Total: page.Total, Counts: page.Counts}
}

func wireIssueLock(lock model.IssueLock) api.IssueLockResponse {
	return api.IssueLockResponse{
		ID: lock.IssueID, Owner: lock.Owner,
		ExpiresAt: lock.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Renewed:   true,
	}
}

// MutatingTool reports the MCP tools behind the audit mutation gate. Clients
// use it to prove their automatic reconnect allowlist holds no mutation.
func MutatingTool(name string) bool {
	switch name {
	case "uplink_invite", "uplink_cert", "client_renew", "uplink_revoke", "uplink_rotate", "access_dismiss":
		return true
	case "issue_unlock", "issue_close_many":
		return true
	case "sync", "promote", "promote_retry", "publish_retry", "publication_abandon", "run_rerun", "run_cancel", "run_purge", "run_hold_exec", "run_hold_patch", "run_continue", "run_hold_abandon", "issue_create", "issue_update", "issue_append", "issue_close", "issue_reopen", "issue_delete", "issue_lock",
		"access_allow", "access_revoke",
		"workflow_capability_grant", "workflow_capability_revoke",

		"release_wif_grant", "release_wif_revoke", "release_wif_reset",
		"repo_remove", "repo_archive", "repo_unarchive", "branch_delete_many", "branch_deletion_resolve", "ref_delete_many", "receive_outbox",
		"secretstore_sync_receipt",
		"promote_grant", "promote_grant_revoke",
		"schedule_pause", "schedule_resume", "schedule_trigger":
		return true
	default:
		return false
	}
}

func (service *API) requireMutation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if service.mutationGate == nil {
		return nil
	}
	if err := service.mutationGate(ctx); err != nil {
		// Busy includes clean foreign-writer re-verification exhaustion, including
		// a Manager deadline wrapping that contention. Keep the public retry reason.
		if errors.Is(err, store.ErrAuditVerificationBusy) {
			return refuse(fmt.Errorf("%w: %w", ErrUnavailable, err), "transient audit verification contention; retry the request")
		}
		return refuse(fmt.Errorf("%w: audit integrity is unavailable: %w", ErrUnavailable, err),
			"audit integrity could not be verified; retry when the audit gate is available")
	}
	return nil
}

func (service *API) resolveRun(ctx context.Context, repositoryName, selector string) (model.Repository, model.Run, error) {
	return service.resolveRunWith(ctx, repositoryName, selector, service.runs.ResolveRun)
}

func (service *API) resolveRunWith(ctx context.Context, repositoryName, selector string, resolve func(context.Context, int64, string) (model.Run, error)) (model.Repository, model.Run, error) {
	if strings.TrimSpace(selector) == "" {
		return model.Repository{}, model.Run{}, fmt.Errorf("%w: run selector is required", ErrInvalidInput)
	}
	if strings.TrimSpace(repositoryName) != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return model.Repository{}, model.Run{}, err
		}
		run, err := resolve(ctx, repository.ID, selector)
		return repository, run, err
	}
	repositories, err := service.runs.ListRepositories(ctx)
	if err != nil {
		return model.Repository{}, model.Run{}, err
	}
	var matchedRepository model.Repository
	var matchedRun model.Run
	var matches []model.Repository
	for _, repository := range repositories {
		run, resolveErr := resolve(ctx, repository.ID, selector)
		if errors.Is(resolveErr, store.ErrNotFound) {
			continue
		}
		if resolveErr != nil {
			return model.Repository{}, model.Run{}, resolveErr
		}
		matches = append(matches, repository)
		matchedRepository, matchedRun = repository, run
	}
	if len(matches) > 1 {
		return model.Repository{}, model.Run{}, service.ambiguousRepositories(ctx, selector, matches)
	}
	if matchedRepository.ID == 0 {
		return model.Repository{}, model.Run{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
	}
	return matchedRepository, matchedRun, nil
}

func (service *API) waitDuration(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, fmt.Errorf("%w: timeout must not be negative", ErrInvalidInput)
	}
	if seconds == 0 {
		return service.maximumWait, nil
	}
	// Compare before converting to Duration: a huge integer must not wrap and
	// bypass the maximum with a negative or deceptively small duration.
	if seconds > int(DefaultMaximumWait/time.Second) || time.Duration(seconds)*time.Second > service.maximumWait {
		return 0, fmt.Errorf("%w: timeout exceeds maximum %s", ErrInvalidInput, service.maximumWait)
	}
	return time.Duration(seconds) * time.Second, nil
}

// canonicalAccessRepo resolves any accepted repository spelling (bare,
// org/repo, upstream/org/repo) onto the qualified persisted grant key the
// v12 migration and the reconciler converge on. An ambiguous bare spelling
// is refused — it names neither same-name repository. An unregistered or
// syntactically unresolvable spelling is returned verbatim: grants for
// unregistered repositories are stored and listed as spelled, and stay
// inert until the repository registers (#245 BLOCKER B, #256).
func (service *API) canonicalAccessRepo(ctx context.Context, repo string) (string, error) {
	if service.repositories == nil {
		return repo, nil
	}
	key, err := canonicalGrantRepoKey(ctx, repo, service.repositories.RepositoryByName, service.secretAccess.QualifiedRepoName)
	if errors.Is(err, store.ErrAmbiguous) {
		return "", fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if err != nil {
		return "", err
	}
	return key, nil
}

func (service *API) accessList(ctx context.Context, repo string, includeRevoked bool) (api.AccessListResponse, error) {
	if service.secretAccess == nil {
		return api.AccessListResponse{}, refuse(ErrUnavailable, "secret access")
	}
	// A non-empty filter accepts every repository spelling; persisted rows
	// key on the qualified form, so a bare filter passed through verbatim
	// would string-match nothing and misreport an empty grant set (#256).
	if strings.TrimSpace(repo) != "" {
		canonical, err := service.canonicalAccessRepo(ctx, repo)
		if err != nil {
			return api.AccessListResponse{}, err
		}
		repo = canonical
	}
	grants, err := service.secretAccess.SecretAccessList(ctx, repo, includeRevoked)
	if err != nil {
		return api.AccessListResponse{}, err
	}
	response := api.AccessListResponse{Grants: make([]api.AccessGrantResponse, len(grants))}
	for index, grant := range grants {
		response.Grants[index] = wireAccessGrant(grant)
	}
	return response, nil
}

// accessListPending returns declared-but-not-granted paths observed at Workflow
// admission. A declaration appears when a pipeline's `oberth secretstore exec`
// references a path; it disappears when an active grant covers that (repo, step,
// path) — either an exact step match or a wildcard grant. Pending never
// auto-approves. Issue #623, finding 4.
func (service *API) accessListPending(ctx context.Context, repo string) (api.AccessListPendingResponse, error) {
	if service.secretAccess == nil {
		return api.AccessListPendingResponse{}, refuse(ErrUnavailable, "secret access")
	}
	if strings.TrimSpace(repo) != "" {
		canonical, err := service.canonicalAccessRepo(ctx, repo)
		if err != nil {
			return api.AccessListPendingResponse{}, err
		}
		repo = canonical
	}
	pending, err := service.secretAccess.PendingGrantDeclarations(ctx, repo)
	if err != nil {
		return api.AccessListPendingResponse{}, err
	}
	response := api.AccessListPendingResponse{
		Declarations: make([]api.PendingDeclaration, len(pending)),
	}
	for i, p := range pending {
		response.Declarations[i] = api.PendingDeclaration{
			Repo:        p.Repo,
			Workflow:    p.Workflow,
			Step:        p.Step,
			Path:        p.Path,
			LastSeenSHA: p.LastSeenSHA,
			LastSeenAt:  p.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}
	return response, nil
}

func (service *API) accessAllow(ctx context.Context, actor api.Actor, repo, workflow, step, secret string) (api.AccessGrantResponse, error) {
	if !actor.Admin {
		return api.AccessGrantResponse{}, refuse(ErrForbidden, "access_allow requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub")
	}
	if service.secretAccess == nil {
		return api.AccessGrantResponse{}, refuse(ErrUnavailable, "secret access")
	}
	if strings.TrimSpace(workflow) == "" || strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: repo, workflow, step, and secret are required", ErrInvalidInput)
	}
	// Resolve the repo name to its qualified form so same-name repos under
	// different upstreams cannot alias each other's grants (#245 BLOCKER B).
	canonical, err := service.canonicalAccessRepo(ctx, repo)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	repo = canonical
	if service.scope != nil && !service.scope.seesAccessKey(repo) {
		return api.AccessGrantResponse{}, hiddenRepository(repo)
	}
	// Unregistered spellings keep their verbatim form above; only a
	// registered, archived repository is refused here.
	if service.repositories != nil {
		if resolved, err := service.repositories.RepositoryByName(ctx, repo); err == nil {
			if err := refuseArchivedGrant(resolved, "access_allow", repo); err != nil {
				return api.AccessGrantResponse{}, err
			}
		}
	}
	// Validate the entry before touching the ConfigMap. Without this check a
	// malformed entry (glob characters in repo/secret, or a non-wildcard step)
	// would be written to the ConfigMap, and the next reconciliation would
	// reject the entire list and revoke every active approval — a single
	// malformed request causing global secret revocation.
	if err := ValidateGrantEntry(0, SecretAccessGrantEntry{Repo: repo, Workflow: workflow, Step: step, Secret: secret}); err != nil {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if service.secretAccessReconciler != nil {
		if err := service.secretAccessReconciler.UpdateConfigMap(ctx, actor.Identity, service.secretAccessReconciler.AddGrantCanonical(ctx, repo, workflow, step, secret)); err != nil {
			return api.AccessGrantResponse{}, fmt.Errorf("update ConfigMap: %w", err)
		}
		// Read the grant back from sqlite after reconcile.
		grants, err := service.secretAccess.SecretAccessList(ctx, repo, false)
		if err != nil {
			return api.AccessGrantResponse{}, err
		}
		for _, grant := range grants {
			if grant.Repo == repo && grant.Workflow == workflow && grant.Step == step && grant.Secret == secret {
				response := wireAccessGrant(grant)
				response.Warning = grantPolicySyncAdvisory
				return response, nil
			}
		}
		return api.AccessGrantResponse{}, fmt.Errorf("grant was written to ConfigMap but not found in store after reconcile")
	}
	grant, err := service.secretAccess.Grant(ctx, repo, workflow, step, secret, actor.Identity)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	response := wireAccessGrant(grant)
	response.Warning = grantPolicySyncAdvisory
	return response, nil
}

func (service *API) accessRevoke(ctx context.Context, actor api.Actor, repo, workflow, step, secret string) (api.AccessGrantResponse, error) {
	if !actor.Admin {
		return api.AccessGrantResponse{}, refuse(ErrForbidden, "access_revoke requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub")
	}
	if service.secretAccess == nil {
		return api.AccessGrantResponse{}, refuse(ErrUnavailable, "secret access")
	}
	if strings.TrimSpace(workflow) == "" || strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: repo, workflow, step, and secret are required", ErrInvalidInput)
	}
	if err := model.ValidateWorkflowFile(workflow); err != nil {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	// Resolve the repo name to its qualified form for identity-safe
	// lookup (#245 BLOCKER B).
	canonical, err := service.canonicalAccessRepo(ctx, repo)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	repo = canonical
	if service.scope != nil && !service.scope.seesAccessKey(repo) {
		return api.AccessGrantResponse{}, hiddenRepository(repo)
	}
	if service.secretAccessReconciler != nil {
		// Read the grant before removal so we can return it.
		grants, err := service.secretAccess.SecretAccessList(ctx, repo, false)
		if err != nil {
			return api.AccessGrantResponse{}, err
		}
		var target *store.SecretAccessGrant
		for i := range grants {
			if grants[i].Repo == repo && grants[i].Workflow == workflow && grants[i].Step == step && grants[i].Secret == secret {
				target = &grants[i]
				break
			}
		}
		if target == nil {
			return api.AccessGrantResponse{}, fmt.Errorf("%w: no active grant for %s workflow %s step %s secret %s", ErrInvalidInput, repo, workflow, step, secret)
		}
		if err := service.secretAccessReconciler.UpdateConfigMap(ctx, actor.Identity, service.secretAccessReconciler.RemoveGrantCanonical(ctx, repo, workflow, step, secret)); err != nil {
			return api.AccessGrantResponse{}, fmt.Errorf("update ConfigMap: %w", err)
		}
		// Read the revoked grant back.
		revoked, err := service.secretAccess.SecretAccessList(ctx, repo, true)
		if err != nil {
			return api.AccessGrantResponse{}, err
		}
		for _, grant := range revoked {
			if grant.ID == target.ID && grant.RevokedAt != nil {
				response := wireAccessGrant(grant)
				response.Warning = revokePolicySyncAdvisory
				return response, nil
			}
		}
		return api.AccessGrantResponse{}, refuse(ErrUnavailable, "revocation was requested but its committed state could not be confirmed; inspect access_list")
	}
	grant, err := service.secretAccess.Revoke(ctx, repo, workflow, step, secret, actor.Identity)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	response := wireAccessGrant(grant)
	response.Warning = revokePolicySyncAdvisory
	return response, nil
}

func (service *API) repoRemove(ctx context.Context, actor api.Actor, repo string) (any, error) {
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "repo_remove requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub")
	}
	if service.repositoryRemover == nil {
		return nil, refuse(ErrUnavailable, "repository removal")
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	}
	removed, err := service.repositoryRemover.RemoveRepository(ctx, actor.Identity, repo)
	if err != nil {
		return nil, err
	}
	response := map[string]string{"removed": removed.Name, "upstream": removed.UpstreamName}
	// Best-effort git cache cleanup: the mapping is already gone, a stale
	// directory is inert, and the next push's Ensure recreates it. A failure
	// is still surfaced to the (admin) caller rather than swallowed, so an
	// operator knows a manual sweep is needed instead of discovering a stale
	// directory later. The cache path derives from the store's registered
	// name, never from raw tool input.
	if service.removeGitCache != nil {
		org := removed.UpstreamOrg
		if org == "" {
			org = removed.UpstreamName
		}
		input := removed.UpstreamName + "/" + org + "/" + removed.Name
		if cacheErr := service.removeGitCache(input); cacheErr != nil {
			response["cache_warning"] = cacheErr.Error()
		}
	}
	return response, nil
}

func wireAccessGrant(grant store.SecretAccessGrant) api.AccessGrantResponse {
	response := api.AccessGrantResponse{
		ID:         grant.ID,
		Repo:       grant.Repo,
		Workflow:   grant.Workflow,
		Step:       grant.Step,
		Secret:     grant.Secret,
		ApprovedBy: grant.ApprovedBy,
		ApprovedAt: grant.ApprovedAt.UTC().Format(time.RFC3339),
		RevokedBy:  grant.RevokedBy,
	}
	if grant.RevokedAt != nil {
		formatted := grant.RevokedAt.UTC().Format(time.RFC3339)
		response.RevokedAt = &formatted
	}
	return response
}

// publishRetry retries the upstream publication for a branch or tag run whose
// burns all passed but whose upstream push failed. A promotion ID, or the ID
// of a promotion's CI run, is refused with a pointer to promote_retry (#1097).
//
// Preconditions (all fail closed with ErrInvalidInput):
//   - The run/promotion is terminal.
//   - All burns/steps passed (the failure is in the publishing phase only).
//   - The run's phase is "publishing" (it failed during upstream publication).
//   - The recorded SHA still resolves in Oberth's git cache.
//
// On success the run transitions to passed and the CI issue auto-closes.
// On failure the error is stored and the run stays retryable.
// Idempotent: if upstream already has the object, succeeds without pushing.
func (service *API) publishRetry(ctx context.Context, actor api.Actor, id string) (api.PublishRetryResponse, error) {
	if service.promotions == nil || service.enqueues == nil || service.auditor == nil {
		return api.PublishRetryResponse{}, refuse(ErrUnavailable, "publish retry")
	}
	if strings.TrimSpace(id) == "" {
		return api.PublishRetryResponse{}, fmt.Errorf("%w: run or promotion ID is required", ErrInvalidInput)
	}

	run, runErr := model.Run{}, store.ErrNotFound
	if service.history != nil {
		run, runErr = service.resolveReadRunID(ctx, id)
	}
	promotion, promErr := service.resolveReadPromotionID(ctx, id)
	if runErr != nil && !errors.Is(runErr, store.ErrNotFound) && !errors.Is(runErr, ErrInvalidInput) {
		return api.PublishRetryResponse{}, runErr
	}
	if promErr != nil && !errors.Is(promErr, store.ErrNotFound) && !errors.Is(promErr, ErrInvalidInput) {
		return api.PublishRetryResponse{}, promErr
	}
	if runErr == nil && promErr == nil {
		return api.PublishRetryResponse{}, fmt.Errorf("%w: ID %q matches run %s and promotion %s; use an unambiguous full ID", store.ErrAmbiguous, id, run.ID, promotion.ID)
	}
	if runErr != nil {
		if promErr == nil {
			return api.PublishRetryResponse{}, promotionPublishRetryRefusal(promotion)
		}
		return api.PublishRetryResponse{}, fmt.Errorf("%w: %q is not a known run or promotion ID", ErrInvalidInput, id)
	}
	if run.Trigger == "promotion" {
		if promotion, err := service.promotions.PromotionByRun(ctx, run.ID); err == nil {
			return api.PublishRetryResponse{}, promotionPublishRetryRefusal(promotion)
		}
		return api.PublishRetryResponse{}, fmt.Errorf("%w: run %s is promotion CI; retry its promotion with promote_retry", ErrInvalidInput, run.ID)
	}
	if run.Status != model.RunFailed || (run.Phase != "publishing" && !pushretry.PublicationFailure(run.Error)) {
		return api.PublishRetryResponse{}, fmt.Errorf("%w: run %s is %s/%s, not publishing-failed; retry is only valid for runs that failed during upstream publication", ErrInvalidInput, run.ID, run.Status, run.Phase)
	}
	runID := run.ID

	// Verify the mutation gate before reactivating any state. A gate failure
	// must leave the run in failed/publishing (retryable) with a typed error,
	// never transition it to running.
	if err := service.requireMutation(ctx); err != nil {
		return api.PublishRetryResponse{}, err
	}

	// Atomically reactivate the run and its exact publication intent.
	publication, err := service.promotions.RetryFailedPublication(ctx, runID, "", actor.Identity)
	if err != nil {
		if IsTransientDatabaseError(err) || strings.Contains(err.Error(), "commit publication retry") {
			if publication.ID != "" {
				service.enqueues.NotifyPublicationRecovery(publication.ID)
			}
			return api.PublishRetryResponse{Action: "unknown", Message: "Publication retry admission is unconfirmed; inspect run_get before retrying", RunID: runID, PublicationID: publication.ID, Status: "unknown", RunStatus: "unknown"}, err
		}
		return api.PublishRetryResponse{}, err
	}

	service.signals.NotifyRun(runID)
	// Deliver the publication through the existing path.
	deliveryCtx, cancelDelivery := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
	_, deliveryErr := service.enqueues.DeliverPublication(deliveryCtx, publication.ID)
	cancelDelivery()

	response := api.PublishRetryResponse{Action: "publication_retry", RunID: runID, PublicationID: publication.ID}
	// Re-read the durable publication and run together even when delivery
	// returned without error. A stale/lost reply must not invent an outcome.
	readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	current, readErr := service.promotions.PublicationState(readCtx, publication.ID)
	cancelRead()
	if readErr != nil {
		service.enqueues.NotifyPublicationRecovery(publication.ID)
		response.Status, response.RunStatus = "unknown", "unknown"
		response.Message = "Publication retry started for existing run " + runID + "; current status unavailable; inspect run_get"
		response.Error = fmt.Sprintf("read publication retry status: %v", readErr)
		return response, nil
	}
	response.Status = string(current.Publication.Status)
	response.RunStatus, response.RunPhase = string(visibleRun(current.Run).Status), current.Run.Phase
	response.Message = "Publication retry started for existing run " + runID + "; "
	switch current.Publication.Status {
	case model.PublicationPending:
		service.enqueues.NotifyPublicationRecovery(publication.ID)
		response.Message += "publication queued; run is " + response.RunStatus + "/" + response.RunPhase
		response.Error = current.Publication.Error
		if response.Error == "" && deliveryErr != nil {
			response.Error = "publication queued for recovery; follow run_get for the result"
		}
	case model.PublicationDelivered:
		response.Message += "publication delivered; run is " + response.RunStatus + "/" + response.RunPhase
	case model.PublicationFailed:
		response.Message += "publication failed; run is " + response.RunStatus + "/" + response.RunPhase
		response.Error = current.Publication.Error
	case model.PublicationAbandoned:
		response.Message += "publication abandoned; run is " + response.RunStatus + "/" + response.RunPhase
		response.Error = current.Publication.Error
	}
	service.signals.NotifyRun(runID)
	service.enqueues.NotifyQueue()
	return response, nil
}

// promotionPublishRetryRefusal explains why publish_retry never re-pushes a
// promotion (#1097): its merge was made against a target head that may have
// moved, and the promotion row is immutable once terminal. promote_retry
// re-merges onto the current head and re-tests instead.
func promotionPublishRetryRefusal(promotion model.Promotion) error {
	if promotion.Status == model.PromotionPassed {
		return fmt.Errorf("%w: promotion %s already delivered %s", ErrInvalidInput, promotion.ID, promotion.TargetRef)
	}
	failure := ""
	if text := strings.TrimSpace(promotion.Error); text != "" {
		failure = " (it failed with: " + strings.ReplaceAll(text, "\n", "; ") + ")"
	}
	return fmt.Errorf("%w: promotion publications are not re-pushed; promote_retry {\"id\":%q} merges promotion %s onto the current %s head and re-tests%s",
		ErrInvalidInput, promotion.ID, promotion.ID, promotion.TargetRef, failure)
}

func decodeTool(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(raw) > maximumToolBytes {
		return fmt.Errorf("%w: tool arguments exceed 1 MiB", ErrInvalidInput)
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(raw), maximumToolBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return refuse(ErrInvalidInput, "%s; accepted arguments: %s", strings.TrimPrefix(err.Error(), "json: "), strings.Join(toolArgumentNames(value), ", "))
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			expected := typeErr.Type.String()
			switch expected {
			case "bool":
				expected = "boolean"
			case "int", "int64":
				expected = "integer"
				// Log arguments carry pattern; list limits are page sizes.
				if _, ok := reflect.TypeOf(value).Elem().FieldByName("Pattern"); ok &&
					(typeErr.Field == "limit" || typeErr.Field == "context" || typeErr.Field == "offset") {
					expected = "non-negative integer line count"
				}
			}
			return refuse(ErrInvalidInput, "%s must be %s", typeErr.Field, expected)
		}
		return fmt.Errorf("%w: decode tool arguments: %w", ErrInvalidInput, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: tool arguments must contain one JSON object", ErrInvalidInput)
	}
	return nil
}

func noRunBranchStatus(repo model.Repository, ref, sha string) StatusResponse {
	return StatusResponse{Repository: repo, Repo: repo.Name, Ref: ref, SHA: sha, Status: "no-runs", Stages: map[string]string{}}
}
