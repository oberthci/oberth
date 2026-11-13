package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/holdfs"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// Agent-assisted continuation tools (run_hold_list, run_hold_exec,
// run_hold_patch, run_continue, run_hold_abandon). See
// docs/assisted-continuation.md. Every mutation is allowed only to an uplink
// that may push to the run's repository (its scope covers it) or an admin,
// only on an active branch-CI run that asked for
// assistance, is serialized per run, capped in concurrency, bounded in time
// and bytes, and appended to the run's audited assist record.

// AssistHoldOperator is the engine side of assisted continuation.
type AssistHoldOperator interface {
	AssistPolicy() model.AssistPolicy
	ListAssistHolds(ctx context.Context, workflowName string) ([]model.AssistHold, error)
	ExecAssistHold(ctx context.Context, request model.AssistExecRequest) (model.AssistExecResult, error)
	ReleaseAssistHold(ctx context.Context, pod string) error
	AbandonAssistHold(ctx context.Context, pod string) error
}

// RunAssistStore appends audited hold actions to a run.
type RunAssistStore interface {
	AppendRunAssist(context.Context, model.RunAssistRequest) (model.Run, error)
}

// maxAssistExecArgs and maxAssistExecArgBytes bound one argv.
const (
	maxAssistExecArgs     = 64
	maxAssistExecArgBytes = 4096
	maxAssistStdinBytes   = 1 << 20
	// assistHoldHelperRoot is the writable source tree of a held attempt.
	assistHoldHelperRoot = "/work/src"
)

// maxConcurrentHoldActions bounds in-flight hold actions (exec, patch,
// continue, abandon) installation-wide. It is not assist.maxConcurrentHolds:
// that caps parked holds, and an action holds no slot of it, so one agent's
// running exec never refuses another agent's continue on its own hold. Each
// run takes one action at a time; this ceiling only guards the Kubernetes API
// against a burst, and is the largest hold cap an installation can set.
const maxConcurrentHoldActions = 64

// assistGate serializes hold actions per run and caps them installation-wide.
// busy holds only the runs with an action in flight, so it never outgrows
// the slot count.
type assistGate struct {
	mu    sync.Mutex
	busy  map[string]bool
	slots chan struct{}
}

func newAssistGate(limit int) *assistGate {
	if limit <= 0 {
		limit = 2
	}
	return &assistGate{busy: map[string]bool{}, slots: make(chan struct{}, limit)}
}

// acquire takes the run's turn and one global slot without waiting: an
// agent that races itself or a saturated installation is refused, not queued.
func (gate *assistGate) acquire(runID string) (func(), error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.busy[runID] {
		return nil, refuse(ErrUnavailable, "another hold action on this run is in progress")
	}
	select {
	case gate.slots <- struct{}{}:
	default:
		return nil, refuse(ErrUnavailable, "the installation's concurrent hold action limit is reached; retry shortly")
	}
	gate.busy[runID] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.mu.Lock()
			delete(gate.busy, runID)
			gate.mu.Unlock()
			<-gate.slots
		})
	}, nil
}

// RunHoldListResponse is run_hold_list.
type RunHoldListResponse struct {
	RunID    string                 `json:"run_id"`
	Step     string                 `json:"failed_step,omitempty"`
	Assisted bool                   `json:"assisted"`
	Policy   model.AssistPolicy     `json:"policy"`
	Holds    []model.AssistHold     `json:"holds"`
	Record   *model.RunAssistRecord `json:"assist,omitempty"`
}

// RunHoldExecResponse is run_hold_exec.
type RunHoldExecResponse struct {
	RunID  string                 `json:"run_id"`
	Step   string                 `json:"step"`
	Pod    string                 `json:"pod"`
	Argv   []string               `json:"argv"`
	Result model.AssistExecResult `json:"result"`
}

// RunHoldPatchResponse is run_hold_patch.
type RunHoldPatchResponse struct {
	RunID       string   `json:"run_id"`
	Step        string   `json:"step"`
	Pod         string   `json:"pod"`
	PatchSHA256 string   `json:"patch_sha256"`
	PatchBytes  int64    `json:"patch_bytes"`
	Files       []string `json:"files"`
}

// RunHoldActionResponse is run_continue and run_hold_abandon.
type RunHoldActionResponse struct {
	RunID    string `json:"run_id"`
	Step     string `json:"step"`
	Pod      string `json:"pod"`
	Action   string `json:"action"`
	Assisted bool   `json:"assisted"`
	Reason   string `json:"reason,omitempty"`
}

// authorizeAssist is the shared gate: the feature is on, the run asked, it
// is an active branch-CI run, and the caller is its uplink or an admin.
func (service *API) authorizeAssist(ctx context.Context, actor api.Actor, id string, mutation bool) (model.Run, error) {
	if service.assistHolds == nil || service.runAssists == nil || service.history == nil {
		return model.Run{}, refuse(ErrUnavailable, "assisted continuation")
	}
	if !service.assistHolds.AssistPolicy().Enabled {
		return model.Run{}, refuse(ErrUnavailable, "assisted continuation is disabled (assist.enabled)")
	}
	if strings.TrimSpace(id) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return model.Run{}, err
	}
	if !run.AssistRequested {
		return model.Run{}, refuse(ErrInvalidInput, "run %s did not ask for assisted continuation (push with -o oberth.assist=1 or rerun with assist)", shortRunID(run.ID))
	}
	if run.RefKind != model.RefBranch || run.Release || (run.Trigger != "branch" && run.Trigger != "push") {
		return model.Run{}, refuse(ErrForbidden, "assisted continuation applies to branch CI runs only")
	}
	if mutation {
		if err := service.authorizeHoldServicer(actor, run); err != nil {
			return model.Run{}, err
		}
	}
	if mutation && !run.Status.Active() {
		return model.Run{}, refuse(ErrInvalidInput, "run %s is %s; nothing is held", shortRunID(run.ID), run.Status)
	}
	return run, nil
}

// authorizeHoldServicer decides who may exec, patch, continue or abandon a
// hold: the permission that already decides who may push to the run's
// repository. An admin; the run's own uplink; or any other uplink whose scope
// covers the run's repository (an unscoped uplink pushes to every one). A
// scoped caller only reaches here through its scoped view, so a run outside
// its scope is already "not found"; the explicit check keeps the rule local.
// The acting identity, not the run's, is what the assist record and audit
// store for every action.
func (service *API) authorizeHoldServicer(actor api.Actor, run model.Run) error {
	if actor.Admin || run.Actor == actor.Identity {
		return nil
	}
	if !actor.Scoped() {
		return nil
	}
	if service.scope == nil || service.scope.actor.Identity != actor.Identity || !service.scope.seesRepo(run.RepoID) {
		return refuse(ErrForbidden, "hold actions on run %s need an uplink whose scope covers its repository, or an admin", shortRunID(run.ID))
	}
	return nil
}

func (service *API) listRunHolds(ctx context.Context, actor api.Actor, id string) (RunHoldListResponse, error) {
	run, err := service.authorizeAssist(ctx, actor, id, false)
	if err != nil {
		return RunHoldListResponse{}, err
	}
	response := RunHoldListResponse{RunID: run.ID, Step: run.FailedStep, Assisted: run.Assisted, Policy: service.assistHolds.AssistPolicy(), Holds: []model.AssistHold{}, Record: run.Assist}
	if run.WorkflowName == "" || !run.Status.Active() {
		return response, nil
	}
	holds, err := service.assistHolds.ListAssistHolds(ctx, run.WorkflowName)
	if err != nil {
		return RunHoldListResponse{}, fmt.Errorf("list holds: %w", err)
	}
	activities := assistActivities(run.Assist)
	for _, hold := range holds {
		hold.Released = assistReleased(run.Assist, hold.Pod)
		withAbandonAt(&hold, activities, response.Policy.HoldTTL)
		response.Holds = append(response.Holds, hold)
	}
	return response, nil
}

func assistActivities(record *model.RunAssistRecord) map[string]model.AssistPodActivity {
	if record == nil {
		return nil
	}
	return record.PodActivities()
}

// withAbandonAt fills the server's abandon deadline of an unreleased hold:
// the sweep's own rule over the run's assist record, so actions that moved
// it are counted.
func withAbandonAt(hold *model.AssistHold, activities map[string]model.AssistPodActivity, ttl time.Duration) {
	if hold.Released || ttl <= 0 || hold.HeldSince.IsZero() {
		return
	}
	at := model.AssistHoldExpiry(*hold, activities[hold.Pod], ttl).UTC()
	hold.AbandonAt = &at
}

func assistReleased(record *model.RunAssistRecord, pod string) bool {
	if record == nil {
		return false
	}
	for _, event := range record.Events {
		if event.Pod == pod && (event.Kind == model.RunAssistContinue || event.Kind == model.RunAssistAbandon) {
			return true
		}
	}
	return false
}

// heldStep resolves the parked attempt of one step; a run with exactly one
// hold may omit the step.
func (service *API) heldStep(ctx context.Context, run model.Run, step string) (model.AssistHold, error) {
	if run.WorkflowName == "" {
		return model.AssistHold{}, refuse(ErrInvalidInput, "run %s has no Workflow yet", shortRunID(run.ID))
	}
	holds, err := service.assistHolds.ListAssistHolds(ctx, run.WorkflowName)
	if err != nil {
		return model.AssistHold{}, fmt.Errorf("list holds: %w", err)
	}
	var parked []model.AssistHold
	for _, hold := range holds {
		if !assistReleased(run.Assist, hold.Pod) && (step == "" || hold.Step == step) {
			parked = append(parked, hold)
		}
	}
	switch {
	case len(parked) == 0 && step == "":
		return model.AssistHold{}, refuse(ErrInvalidInput, "run %s holds no step", shortRunID(run.ID))
	case len(parked) == 0:
		return model.AssistHold{}, refuse(ErrInvalidInput, "run %s does not hold step %q", shortRunID(run.ID), step)
	case len(parked) > 1:
		names := make([]string, 0, len(parked))
		for _, hold := range parked {
			names = append(names, hold.Step)
		}
		return model.AssistHold{}, refuse(ErrInvalidInput, "run %s holds several steps (%s); name one", shortRunID(run.ID), strings.Join(names, ", "))
	}
	return parked[0], nil
}

func (service *API) recordAssist(ctx context.Context, actor api.Actor, run model.Run, event model.RunAssistEvent) (model.Run, error) {
	event.Actor = actor.Identity
	updated, err := service.runAssists.AppendRunAssist(ctx, model.RunAssistRequest{RunID: run.ID, Event: event, Authorize: func(current model.Run, upstream string) error {
		if !current.Status.Active() && event.Kind != model.RunAssistAbandon {
			return fmt.Errorf("%w: run %s is %s", store.ErrInvalidState, shortRunID(current.ID), current.Status)
		}
		if current.RepoID != run.RepoID {
			return fmt.Errorf("%w: run %s changed repository", ErrForbidden, shortRunID(current.ID))
		}
		if err := service.authorizeHoldServicer(actor, current); err != nil {
			return err
		}
		// The scope view was resolved at call start; the repository's upstream
		// is read again in this transaction, so a repository moved out of the
		// caller's scope since then is refused.
		if !actor.Admin && current.Actor != actor.Identity && actor.Scoped() && !actor.SeesUpstream(upstream) {
			return refuse(ErrForbidden, "hold actions on run %s need an uplink whose scope covers its repository, or an admin", shortRunID(current.ID))
		}
		return nil
	}})
	if err != nil {
		return model.Run{}, err
	}
	service.signals.NotifyRun(run.ID)
	return updated, nil
}

func validateAssistArgv(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%w: argv is required", ErrInvalidInput)
	}
	if len(argv) > maxAssistExecArgs {
		return fmt.Errorf("%w: argv has more than %d entries", ErrInvalidInput, maxAssistExecArgs)
	}
	for index, argument := range argv {
		if len(argument) > maxAssistExecArgBytes {
			return fmt.Errorf("%w: argv[%d] is longer than %d bytes", ErrInvalidInput, index, maxAssistExecArgBytes)
		}
		if strings.ContainsRune(argument, 0) {
			return fmt.Errorf("%w: argv[%d] contains NUL", ErrInvalidInput, index)
		}
	}
	if strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("%w: argv[0] is empty", ErrInvalidInput)
	}
	return nil
}

func (service *API) execRunHold(ctx context.Context, actor api.Actor, id, step string, argv []string, stdin string, timeoutSeconds int) (RunHoldExecResponse, error) {
	if err := validateAssistArgv(argv); err != nil {
		return RunHoldExecResponse{}, err
	}
	if len(stdin) > maxAssistStdinBytes {
		return RunHoldExecResponse{}, fmt.Errorf("%w: stdin is larger than %d bytes", ErrInvalidInput, maxAssistStdinBytes)
	}
	run, err := service.authorizeAssist(ctx, actor, id, true)
	if err != nil {
		return RunHoldExecResponse{}, err
	}
	release, err := service.assistGate.acquire(run.ID)
	if err != nil {
		return RunHoldExecResponse{}, err
	}
	defer release()
	hold, err := service.heldStep(ctx, run, step)
	if err != nil {
		return RunHoldExecResponse{}, err
	}
	policy := service.assistHolds.AssistPolicy()
	timeout := policy.ExecTimeout
	if timeoutSeconds > 0 && time.Duration(timeoutSeconds)*time.Second < timeout {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	// Record before running: an exec can change the tree or release the
	// hold itself, so the run is assisted from this moment whatever the
	// command does. The exit code is returned, not recorded.
	if _, err := service.recordAssist(ctx, actor, run, model.RunAssistEvent{Step: hold.Step, Pod: hold.Pod, Kind: model.RunAssistExec, Argv: argv}); err != nil {
		return RunHoldExecResponse{}, err
	}
	result, err := service.assistHolds.ExecAssistHold(ctx, model.AssistExecRequest{Pod: hold.Pod, Argv: argv, Stdin: []byte(stdin), Timeout: timeout, OutputLimit: policy.ExecOutputBytes})
	if err != nil {
		return RunHoldExecResponse{}, fmt.Errorf("exec in held step %s: %w", hold.Step, err)
	}
	return RunHoldExecResponse{RunID: run.ID, Step: hold.Step, Pod: hold.Pod, Argv: argv, Result: result}, nil
}

func (service *API) patchRunHold(ctx context.Context, actor api.Actor, id, step, diff string) (RunHoldPatchResponse, error) {
	if strings.TrimSpace(diff) == "" {
		return RunHoldPatchResponse{}, fmt.Errorf("%w: diff is required", ErrInvalidInput)
	}
	run, err := service.authorizeAssist(ctx, actor, id, true)
	if err != nil {
		return RunHoldPatchResponse{}, err
	}
	policy := service.assistHolds.AssistPolicy()
	patches, err := holdfs.Parse([]byte(diff), holdfs.Limits{MaxBytes: policy.PatchMaxBytes, MaxFiles: policy.PatchMaxFiles})
	if err != nil {
		return RunHoldPatchResponse{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	release, err := service.assistGate.acquire(run.ID)
	if err != nil {
		return RunHoldPatchResponse{}, err
	}
	defer release()
	hold, err := service.heldStep(ctx, run, step)
	if err != nil {
		return RunHoldPatchResponse{}, err
	}
	if !hold.PatchCapable {
		return RunHoldPatchResponse{}, refuse(ErrInvalidInput, "step %s is inspect-only: its held Pod has no server helper mount (%s); run_hold_exec still works", hold.Step, holdfs.HelperMountPath)
	}
	digest := holdfs.Digest([]byte(diff))
	files := make([]string, 0, len(patches))
	for _, patch := range patches {
		files = append(files, patch.Path)
	}
	// Record before applying: the verdict must not depend on whether the
	// write succeeds.
	if _, err := service.recordAssist(ctx, actor, run, model.RunAssistEvent{Step: hold.Step, Pod: hold.Pod, Kind: model.RunAssistPatch, PatchSHA256: digest, PatchBytes: int64(len(diff)), Files: files}); err != nil {
		return RunHoldPatchResponse{}, err
	}
	if _, err := service.applyHoldPatch(ctx, hold, patches, policy); err != nil {
		return RunHoldPatchResponse{}, err
	}
	return RunHoldPatchResponse{RunID: run.ID, Step: hold.Step, Pod: hold.Pod, PatchSHA256: digest, PatchBytes: int64(len(diff)), Files: files}, nil
}

// applyHoldPatch reads the touched files through the Pod helper, applies
// every hunk server-side with exact context, then writes all files with
// their read digests as preconditions; the helper refuses the whole write
// if any file changed in between.
func (service *API) applyHoldPatch(ctx context.Context, hold model.AssistHold, patches []holdfs.FilePatch, policy model.AssistPolicy) ([]string, error) {
	paths := make([]string, 0, len(patches))
	for _, patch := range patches {
		paths = append(paths, patch.Path)
	}
	readRequest, err := encodeJSON(holdfs.ReadRequest{Paths: paths, MaxBytes: policy.PatchMaxBytes})
	if err != nil {
		return nil, err
	}
	read, err := service.holdHelper(ctx, hold, []string{"read", "--root", assistHoldHelperRoot}, readRequest, policy)
	if err != nil {
		return nil, err
	}
	var current holdfs.ReadResponse
	if err := decodeJSON(read, &current); err != nil {
		return nil, fmt.Errorf("hold helper read: %w", err)
	}
	if len(current.Files) != len(patches) {
		return nil, errors.New("hold helper read returned the wrong file count")
	}
	write := holdfs.WriteRequest{}
	for index, patch := range patches {
		state := current.Files[index]
		if state.Path != patch.Path {
			return nil, errors.New("hold helper read returned files out of order")
		}
		if state.Truncated {
			return nil, refuse(ErrInvalidInput, "%s is larger than the patch limit (%d bytes)", patch.Path, policy.PatchMaxBytes)
		}
		updated, err := holdfs.Apply(state.Content, state.Exists, patch)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		expect := ""
		if state.Exists {
			expect = state.SHA256
		}
		write.Files = append(write.Files, holdfs.FileWrite{Path: patch.Path, ExpectSHA256: expect, Content: updated, Delete: patch.Delete})
	}
	writeRequest, err := encodeJSON(write)
	if err != nil {
		return nil, err
	}
	written, err := service.holdHelper(ctx, hold, []string{"write", "--root", assistHoldHelperRoot}, writeRequest, policy)
	if err != nil {
		return nil, err
	}
	var response holdfs.WriteResponse
	if err := decodeJSON(written, &response); err != nil {
		return nil, fmt.Errorf("hold helper write: %w", err)
	}
	return paths, nil
}

func (service *API) holdHelper(ctx context.Context, hold model.AssistHold, args []string, stdin []byte, policy model.AssistPolicy) ([]byte, error) {
	argv := append([]string{holdfs.HelperPath, "internal-hold-fs"}, args...)
	result, err := service.assistHolds.ExecAssistHold(ctx, model.AssistExecRequest{Pod: hold.Pod, Argv: argv, Stdin: stdin, Timeout: policy.ExecTimeout, OutputLimit: max(policy.PatchMaxBytes*2+(1<<20), policy.ExecOutputBytes)})
	if err != nil {
		return nil, fmt.Errorf("hold helper: %w", err)
	}
	if result.TimedOut {
		return nil, refuse(ErrUnavailable, "hold helper timed out in the held pod")
	}
	if result.ExitCode != 0 {
		return nil, refuse(ErrInvalidInput, "hold helper refused: %s", strings.TrimSpace(result.Stderr))
	}
	if result.Truncated {
		return nil, errors.New("hold helper output exceeded its bound")
	}
	return []byte(result.Stdout), nil
}

func (service *API) continueRunHold(ctx context.Context, actor api.Actor, id, step string) (RunHoldActionResponse, error) {
	run, err := service.authorizeAssist(ctx, actor, id, true)
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	release, err := service.assistGate.acquire(run.ID)
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	defer release()
	hold, err := service.heldStep(ctx, run, step)
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	// Record first: a continued step must never run without the evidence
	// exclusion. If the release itself fails, the Pod is abandoned so the
	// step fails normally instead of lingering in an unknown state; the run
	// stays assisted, which is the conservative verdict.
	updated, err := service.recordAssist(ctx, actor, run, model.RunAssistEvent{Step: hold.Step, Pod: hold.Pod, Kind: model.RunAssistContinue})
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	if err := service.assistHolds.ReleaseAssistHold(ctx, hold.Pod); err != nil {
		abandonErr := service.assistHolds.AbandonAssistHold(ctx, hold.Pod)
		if _, recordErr := service.recordAssist(ctx, actor, run, model.RunAssistEvent{Step: hold.Step, Pod: hold.Pod, Kind: model.RunAssistAbandon}); recordErr != nil {
			abandonErr = errors.Join(abandonErr, recordErr)
		}
		return RunHoldActionResponse{}, fmt.Errorf("release held step %s failed, so its Pod was abandoned and the step fails: %w", hold.Step, errors.Join(err, abandonErr))
	}
	reason := ""
	if updated.Assist != nil {
		reason = updated.Assist.Reason()
	}
	return RunHoldActionResponse{RunID: run.ID, Step: hold.Step, Pod: hold.Pod, Action: model.RunAssistContinue, Assisted: updated.Assisted, Reason: reason}, nil
}

func (service *API) abandonRunHold(ctx context.Context, actor api.Actor, id, step string) (RunHoldActionResponse, error) {
	run, err := service.authorizeAssist(ctx, actor, id, true)
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	release, err := service.assistGate.acquire(run.ID)
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	defer release()
	hold, err := service.heldStep(ctx, run, step)
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	if err := service.assistHolds.AbandonAssistHold(ctx, hold.Pod); err != nil {
		return RunHoldActionResponse{}, fmt.Errorf("abandon held step %s: %w", hold.Step, err)
	}
	updated, err := service.recordAssist(ctx, actor, run, model.RunAssistEvent{Step: hold.Step, Pod: hold.Pod, Kind: model.RunAssistAbandon})
	if err != nil {
		return RunHoldActionResponse{}, err
	}
	return RunHoldActionResponse{RunID: run.ID, Step: hold.Step, Pod: hold.Pod, Action: model.RunAssistAbandon, Assisted: updated.Assisted}, nil
}

func encodeJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode hold helper request: %w", err)
	}
	return encoded, nil
}

func decodeJSON(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}
