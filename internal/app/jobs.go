package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/job"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runner"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type jobControl interface {
	Create(context.Context, job.Request) (string, error)
	Wait(context.Context, string, string, io.Writer, [][]byte, []byte) (job.Completion, error)
	Cancel(context.Context, string, string) error
	TerminalState(context.Context, string) (*job.Completion, error)
}

type releaseSecretControl interface {
	SecretSnapshot(context.Context, string, []string) (job.SecretSnapshot, error)
}

type secretStoreControl interface {
	SecretStoreSnapshot(context.Context, []periapsis.SecretStoreDeclaration, []string) (job.SecretStoreSnapshot, error)
}

type jobIntent struct {
	runID        string
	testedSHA    string
	baseSHA      string
	secretMask   [][]byte
	storePayload []byte
}

// zero overwrites the secret material an intent carried. Slices are shared
// with the live Wait copy, so callers only invoke it once the run's masking
// and delivery lifetime has ended.
func (intent jobIntent) zero() {
	for _, value := range intent.secretMask {
		job.ZeroBytes(value)
	}
	job.ZeroBytes(intent.storePayload)
}

// Jobs is the concrete capability split between ordinary CI and release Jobs.
// Release=true is never caller-controlled: only CreateRelease can set it and
// load the release-secret masking values.
type Jobs struct {
	controller    jobControl
	auditor       service.Auditor
	workspaceRoot string
	mu            sync.Mutex
	intents       map[string]jobIntent
}

func NewJobs(controller jobControl, workspaceRoot string, auditor service.Auditor) (*Jobs, error) {
	if controller == nil {
		return nil, errors.New("app: Job controller is required")
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		workspaceRoot = "/data/work"
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	if !filepath.IsAbs(workspaceRoot) {
		return nil, errors.New("app: Job workspace root must be absolute")
	}
	return &Jobs{controller: controller, auditor: auditor, workspaceRoot: workspaceRoot, intents: make(map[string]jobIntent)}, nil
}

func (jobs *Jobs) CreateCI(ctx context.Context, request service.JobRequest) error {
	return jobs.create(ctx, request, false)
}

func (jobs *Jobs) CreateRelease(ctx context.Context, request service.JobRequest) error {
	return jobs.create(ctx, request, true)
}

func (jobs *Jobs) create(ctx context.Context, request service.JobRequest, release bool) error {
	// Serialize request preparation and Create with Delete. Release preparation
	// can contact Kubernetes to freeze Secrets, so locking only after request
	// construction leaves a superseder able to delete a not-yet-created name,
	// complete its durable cancellation, and race a later Create.
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	internal, intent, err := jobs.request(ctx, request, release)
	if err != nil {
		return err
	}
	created, err := jobs.controller.Create(ctx, internal)
	if err != nil {
		intent.zero()
		return err
	}
	if created != request.JobName {
		_ = jobs.controller.Cancel(ctx, created, intent.runID)
		intent.zero()
		return fmt.Errorf("app: Kubernetes created Job %q, expected %q", created, request.JobName)
	}
	jobs.intents[created] = intent
	return nil
}

func (jobs *Jobs) request(ctx context.Context, request service.JobRequest, release bool) (job.Request, jobIntent, error) {
	if strings.TrimSpace(request.JobName) == "" || strings.TrimSpace(request.Run.ID) == "" || request.Repository.Name == "" {
		return job.Request{}, jobIntent{}, errors.New("app: deterministic Job name, run, and repository are required")
	}
	expectedSource := filepath.Join(jobs.workspaceRoot, request.Run.ID, "src")
	if request.SourceDir != filepath.Clean(request.SourceDir) || request.SourceDir != expectedSource {
		return job.Request{}, jobIntent{}, errors.New("app: Job source directory does not match its durable run")
	}
	testedSHA := request.Run.TestedSHA
	if testedSHA == "" {
		testedSHA = request.Run.SHA
	}
	intent := jobIntent{runID: request.Run.ID, testedSHA: testedSHA, baseSHA: request.Run.BaseSHA}
	var releaseSecrets *job.SecretSnapshot
	var vaultSources []job.SecretStoreSource
	if release {
		source, err := readPeriapsisSource(request.SourceDir)
		if err != nil {
			return job.Request{}, jobIntent{}, fmt.Errorf("app: read repository release Secret contract: %w", err)
		}
		requested, err := periapsis.DeclaredReleaseSecrets(source)
		if err != nil {
			return job.Request{}, jobIntent{}, fmt.Errorf("app: read repository release Secret contract: %w", err)
		}
		storeDeclared, err := periapsis.DeclaredSecretStoreSecrets(source)
		if err != nil {
			return job.Request{}, jobIntent{}, fmt.Errorf("app: read repository secret store contract: %w", err)
		}
		secretControl, ok := jobs.controller.(releaseSecretControl)
		if !ok {
			return job.Request{}, jobIntent{}, errors.New("app: release Secret snapshot capability is required")
		}
		snapshot, err := secretControl.SecretSnapshot(ctx, request.JobName, requested)
		if err != nil {
			return job.Request{}, jobIntent{}, fmt.Errorf("app: load release Secret snapshot: %w", err)
		}
		releaseSecrets = &snapshot
		intent.secretMask = snapshot.MaskValues()
		if len(storeDeclared) != 0 {
			storeSnapshot, err := jobs.fetchSecretStoreSecrets(ctx, request, snapshot, storeDeclared)
			if err != nil {
				return job.Request{}, jobIntent{}, err
			}
			payload, err := storeSnapshot.DeliveryPayload()
			if err != nil {
				storeSnapshot.Zero()
				return job.Request{}, jobIntent{}, fmt.Errorf("app: encode secret store delivery: %w", err)
			}
			intent.secretMask = append(intent.secretMask, storeSnapshot.MaskValues()...)
			intent.storePayload = payload
			vaultSources = storeSnapshot.Sources()
			// The payload and mask own their copies; the fetch-time snapshot is
			// no longer needed anywhere.
			storeSnapshot.Zero()
		}
	}
	return job.Request{
		RunID: request.Run.ID, JobName: request.JobName, Repo: request.Repository.Name,
		Ref: request.Run.Ref, SHA: testedSHA, Release: release, Trusted: request.Run.Trigger == "promotion",
		ReleaseSecrets: releaseSecrets, SecretStoreSecrets: vaultSources,
	}, intent, nil
}

// fetchSecretStoreSecrets performs the fail-closed, admission-time secret store fetch: the
// audited intent binds the acting uplink identity before OpenBao is contacted,
// and any unavailable path fails the release before its Job exists.
func (jobs *Jobs) fetchSecretStoreSecrets(
	ctx context.Context,
	request service.JobRequest,
	snapshot job.SecretSnapshot,
	declared []periapsis.SecretStoreDeclaration,
) (job.SecretStoreSnapshot, error) {
	storeControl, ok := jobs.controller.(secretStoreControl)
	if !ok {
		return job.SecretStoreSnapshot{}, errors.New("app: secret store secret snapshot capability is required")
	}
	if jobs.auditor == nil {
		return job.SecretStoreSnapshot{}, errors.New("app: secret store fetches require the audit chain")
	}
	actor := strings.TrimSpace(request.Run.Actor)
	if actor == "" {
		return job.SecretStoreSnapshot{}, errors.New("app: secret store fetches require an attributable uplink identity")
	}
	names := make([]string, 0, len(declared))
	paths := make([]string, 0, len(declared))
	for _, declaration := range declared {
		names = append(names, declaration.Name)
		paths = append(paths, declaration.Path)
	}
	details := map[string]any{
		"repo": request.Repository.Name, "ref": request.Run.Ref, "sha": request.Run.SHA,
		"job": request.JobName, "names": names, "paths": paths,
	}
	if err := jobs.appendSecretStoreAudit(ctx, actor, request.Run.ID, "release.secretstore.fetch.intent", details); err != nil {
		return job.SecretStoreSnapshot{}, fmt.Errorf("app: persist secret store secret fetch intent: %w", err)
	}
	kubernetesNames := make([]string, 0, len(snapshot.Mounts.Secrets))
	for _, secret := range snapshot.Mounts.Secrets {
		kubernetesNames = append(kubernetesNames, secret.Name)
	}
	storeSnapshot, fetchErr := storeControl.SecretStoreSnapshot(ctx, declared, kubernetesNames)
	outcome := map[string]any{"outcome": "succeeded"}
	action := "release.secretstore.fetch.succeeded"
	if fetchErr != nil {
		outcome = map[string]any{"outcome": "failed", "error": fetchErr.Error()}
		action = "release.secretstore.fetch.failed"
	}
	for key, value := range details {
		outcome[key] = value
	}
	auditErr := jobs.appendSecretStoreAudit(ctx, actor, request.Run.ID, action, outcome)
	if auditErr != nil {
		auditErr = fmt.Errorf("app: persist secret store secret fetch outcome: %w", auditErr)
	}
	if fetchErr != nil {
		return job.SecretStoreSnapshot{}, errors.Join(fmt.Errorf("app: load secret store secret snapshot: %w", fetchErr), auditErr)
	}
	if auditErr != nil {
		storeSnapshot.Zero()
		return job.SecretStoreSnapshot{}, auditErr
	}
	return storeSnapshot, nil
}

func (jobs *Jobs) appendSecretStoreAudit(ctx context.Context, actor, runID, action string, details map[string]any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit details: %w", err)
	}
	_, err = jobs.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor, Action: action, ResourceType: "run", ResourceID: runID, Details: string(encoded),
	})
	return err
}

func readPeriapsisSource(sourceDir string) (string, error) {
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return "", fmt.Errorf("open immutable source root: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(".oberth/periapsis.go")
	if err != nil {
		return "", fmt.Errorf("open .oberth/periapsis.go without escaping source root: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat .oberth/periapsis.go: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > periapsis.MaxSourceBytes {
		return "", errors.New(".oberth/periapsis.go must be a bounded regular file")
	}
	source, err := io.ReadAll(io.LimitReader(file, periapsis.MaxSourceBytes+1))
	if err != nil {
		return "", fmt.Errorf("read .oberth/periapsis.go: %w", err)
	}
	if len(source) > periapsis.MaxSourceBytes {
		return "", errors.New(".oberth/periapsis.go exceeds the source-size limit")
	}
	return string(source), nil
}

func (jobs *Jobs) Wait(ctx context.Context, name string, destination io.Writer) (service.JobResult, error) {
	jobs.mu.Lock()
	intent, ok := jobs.intents[name]
	jobs.mu.Unlock()
	if !ok {
		return service.JobResult{}, fmt.Errorf("app: no in-process intent for Job %s", name)
	}
	defer jobs.forget(name, intent.runID)
	completion, err := jobs.controller.Wait(ctx, name, intent.runID, destination, intent.secretMask, intent.storePayload)
	if err != nil {
		return service.JobResult{}, err
	}
	result := service.JobResult{Status: model.RunFailed, Phase: "job", TestedSHA: intent.testedSHA, BaseSHA: intent.baseSHA}
	if completion.Succeeded && completion.ExitCode == 0 {
		if len(completion.Summary) == 0 {
			return service.JobResult{}, errors.New("app: successful Job has no binding step summary")
		}
		result.Status = model.RunPassed
		result.Phase = "passed"
	} else {
		result.Error = humanizeJobReason(strings.TrimSpace(completion.Reason))
		if result.Error == "" {
			result.Error = fmt.Sprintf("runner exited with code %d", completion.ExitCode)
		}
	}
	if len(completion.Summary) != 0 {
		steps, err := decodeModelSteps(completion.Summary)
		if err != nil {
			return service.JobResult{}, err
		}
		if len(steps) == 0 {
			return service.JobResult{}, errors.New("app: Job binding summary has no step results")
		}
		result.Steps = steps
		for _, step := range steps {
			if step.Status == model.StepPassed || step.Status == model.StepSkipped {
				continue
			}
			result.FailedBurn, result.FailedStep = step.Burn, step.Step
			result.Phase = step.Burn
		}
	}
	if result.Status == model.RunPassed && result.FailedStep != "" {
		return service.JobResult{}, errors.New("app: successful Job reported a failed step")
	}
	return result, nil
}

func (jobs *Jobs) Delete(ctx context.Context, name, runID string) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if strings.TrimSpace(runID) == "" {
		return errors.New("app: durable run ID is required to delete a Job")
	}
	if current, ok := jobs.intents[name]; ok && current.runID != runID {
		return fmt.Errorf("app: Job %s belongs to a different in-process run", name)
	}
	if err := jobs.controller.Cancel(ctx, name, runID); err != nil {
		return err
	}
	if current, ok := jobs.intents[name]; ok && current.runID == runID {
		delete(jobs.intents, name)
		current.zero()
	}
	return nil
}

func (jobs *Jobs) forget(name, runID string) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if current, ok := jobs.intents[name]; ok && current.runID == runID {
		delete(jobs.intents, name)
		current.zero()
	}
}

// TerminalResult checks whether a named K8s Job has reached a terminal state
// and maps its completion to a durable JobResult. It does not require an
// in-process intent: the method reads the K8s API directly, making it safe to
// call during startup reconciliation for Jobs created by a previous process.
func (jobs *Jobs) TerminalResult(ctx context.Context, name string) (service.JobResult, error) {
	completion, err := jobs.controller.TerminalState(ctx, name)
	if completion == nil {
		if err != nil {
			return service.JobResult{}, err
		}
		return service.JobResult{}, service.ErrJobNotTerminal
	}
	result := service.JobResult{Status: model.RunFailed, Phase: "job"}
	if completion.Succeeded && completion.ExitCode == 0 {
		result.Status = model.RunPassed
		result.Phase = "passed"
		if len(completion.Summary) == 0 {
			note := "reconciled without runner step evidence"
			if err != nil {
				note += ": " + err.Error()
			}
			result.Error = note
		}
	} else {
		result.Error = humanizeJobReason(strings.TrimSpace(completion.Reason))
		if result.Error == "" {
			result.Error = fmt.Sprintf("runner exited with code %d", completion.ExitCode)
		}
	}
	if len(completion.Summary) != 0 {
		steps, stepsErr := decodeModelSteps(completion.Summary)
		if stepsErr == nil && len(steps) > 0 {
			result.Steps = steps
			for _, step := range steps {
				if step.Status == model.StepPassed || step.Status == model.StepSkipped {
					continue
				}
				result.FailedBurn, result.FailedStep = step.Burn, step.Step
				result.Phase = step.Burn
			}
		}
	}
	if result.Status == model.RunPassed && result.FailedStep != "" {
		result.Status = model.RunFailed
		result.Phase = result.FailedBurn
		result.Error = "reconciled: successful Job reported a failed step"
	}
	return result, nil
}

// humanizeJobReason replaces raw Kubernetes Job condition reasons with
// human-readable text suitable for CI issue bodies.
func humanizeJobReason(reason string) string {
	switch reason {
	case "BackoffLimitExceeded":
		return "Job failed (backoff limit exceeded)"
	case "DeadlineExceeded":
		return "Job failed (deadline exceeded)"
	default:
		return reason
	}
}

func decodeModelSteps(raw json.RawMessage) ([]model.StepResult, error) {
	values, err := runner.DecodeStepResults(raw)
	if err != nil {
		return nil, fmt.Errorf("app: decode validated Job steps: %w", err)
	}
	results := make([]model.StepResult, len(values))
	for index, value := range values {
		// DecodeStepResults already validated both timestamps, so a parse
		// failure here means the runner contract drifted; fail loudly instead
		// of recording zero-value (epoch) timings.
		started, err := time.Parse(time.RFC3339Nano, value.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("app: step %s/%s started_at %q: %w", value.Burn, value.Step, value.StartedAt, err)
		}
		finished, err := time.Parse(time.RFC3339Nano, value.FinishedAt)
		if err != nil {
			return nil, fmt.Errorf("app: step %s/%s finished_at %q: %w", value.Burn, value.Step, value.FinishedAt, err)
		}
		results[index] = model.StepResult{
			Burn: value.Burn, Step: value.Step, Status: model.StepStatus(value.Status), ExitCode: value.ExitCode,
			StartedAt: &started, FinishedAt: &finished,
		}
	}
	return results, nil
}

var _ service.JobController = (*Jobs)(nil)
var _ service.ReleaseJobController = (*Jobs)(nil)
