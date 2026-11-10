package argojob

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/internal/holdfs"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Agent-assisted CI continuation ("retry hold").
//
// A branch-tier run that opted in (`git push -o oberth.assist=1` or
// `run_rerun` with assist) gets, on every credential-free pod leaf, a
// server-owned retryStrategy of one attempt and a server-owned conditional
// podSpecPatch. Attempt 1 runs exactly as today. When attempt 1 exits
// nonzero, Argo creates the retry pod with the patch applied: the emissary
// executor parks before exec'ing the step command (ARGO_DEBUG_PAUSE_BEFORE),
// the kubelet bounds the park with a wider Pod deadline, the source mount is
// writable and the shared caches are detached. The run's agent inspects,
// patches and releases the held pod through the run_hold_* MCP tools; the
// step then runs natively and the DAG continues. An attempt the kubelet
// stopped at its deadline is not a failed command: it is neither retried
// nor held (assistCommandFailure).
//
// Nothing here adds node reach or credentials: the patch only touches the
// main container's env and the read-only flag of two server-owned mounts,
// and only on runs that could never log in to the secret store.
const (
	// AssistHoldAnnotation marks a hold-eligible template and its pods. The
	// value is the Pod deadline in seconds the held attempt receives. Template
	// metadata admission refuses it from repositories; only Build writes it.
	AssistHoldAnnotation = "oberth.ci/assist-hold"
	// AssistWorkflowAnnotation records on the Workflow that the run is in
	// assist mode, with the hold TTL in seconds; Build added
	// AssistHoldMaxAgeFactor of them to the workflow deadline. Build deletes
	// an authored copy before stamping its own.
	AssistWorkflowAnnotation = "oberth.ci/assist"

	// assistPauseEnv is Argo's emissary debug pause (cmd/argoexec/commands/
	// emissary.go): PID 1 waits for /var/run/argo/ctr/main/before.
	assistPauseEnv = "ARGO_DEBUG_PAUSE_BEFORE"
	// AssistHoldEnv tells the step itself that it runs as a held attempt.
	AssistHoldEnv = "OBERTH_ASSIST_HOLD"
	// AssistReleasePath is the file the executor waits for. run_continue
	// creates it through pods/exec with the holdfs helper.
	AssistReleasePath = holdfs.ReleaseMarkerPath
	// AssistScratchCachePath receives the Go caches of a held attempt so a
	// patched tree can never write into the shared, cross-run cache.
	AssistScratchCachePath = "/tmp/oberth-assist-cache"

	// Exit codes of a main container that died by SIGKILL or SIGTERM. The
	// kubelet's deadline kill sends SIGTERM to the emissary, which forwards it
	// to the step command and exits with its code, so the controller records
	// 143 (or 137 after the grace period); it is not the -1 of a Pod with no
	// terminated main container (operator.go getExitCode).
	exitCodeSIGKILL = 137
	exitCodeSIGTERM = 143
)

// assistCommandFailure is the retry-local expression that tells a genuine
// command failure from a kill. The retryStrategy scope (operator.go
// buildRetryStrategyLocalScope) carries lastRetry.exitCode and
// lastRetry.duration for the last attempt, the duration being the node's
// FinishedAt (the container's termination time) minus its StartedAt (node
// creation, before scheduling; a long Pending phase counts). A Pod the kubelet
// stopped at activeDeadlineSeconds has therefore run for at least the
// floored step budget and exited 143 (137 once the grace period ran out); a
// command that failed on its own did so with another code, before the
// budget. Only the latter is retried and held. A command that happens to
// exit 137 or 143 itself, or one that outlives its budget during the
// kubelet's check interval, is not held: conservative, and it keeps #1499
// start-up classification and the run-level infrastructure retry exactly as
// for an unassisted run, as is a genuine failure after a long Pending phase
// that pushed the duration past the budget.
func assistCommandFailure(budget int64) string {
	return fmt.Sprintf("asInt(lastRetry.exitCode) > 0 && asInt(lastRetry.exitCode) != %d && asInt(lastRetry.exitCode) != %d && asFloat(lastRetry.duration) < %d", exitCodeSIGKILL, exitCodeSIGTERM, budget)
}

// assistRetryExpression retries only a genuine command failure (see
// assistCommandFailure). A Pod deadline kill, an eviction or an executor
// error is never retried in the Workflow.
func assistRetryExpression(budget int64) string { return assistCommandFailure(budget) }

// assistPatchCondition selects the held branch of the podSpecPatch
// expression: any retry attempt. Argo creates attempt (N), N >= 1, only
// after processNodeRetries let assistRetryExpression pass (operator.go
// :1158-1168, :2393), so `retries > 0` already implies a failed command.
// The patch must not repeat the discriminator: when the Pod of attempt N
// could not be created at once (quota, transient API error), the controller
// substitutes the patch again for the pending child with lastRetry.exitCode
// 0 and duration 0 (operator.go:2384-2389, :2410-2418), and a repeated
// discriminator would then resolve to the base branch, running the step
// again unheld.
const assistPatchCondition = "asInt(retries) > 0"

// AssistHoldMaxAgeFactor bounds a worked hold: a hold with an agent action
// ages from its last action instead of held_since, but never beyond this
// many hold TTLs after held_since. The held Pod's deadline and the workflow
// deadline therefore grow by AssistHoldMaxAgeFactor TTLs, so a hold
// extended to the cap still has the step's whole budget after release.
const AssistHoldMaxAgeFactor = 2

// assistHoldAllowance is the time a held attempt may spend parked: the
// bound the sweep enforces on a worked hold, added to the Pod and workflow
// deadlines.
func assistHoldAllowance(ttl int64) int64 { return AssistHoldMaxAgeFactor * ttl }

// Administrator bounds for AssistConfig.
const (
	DefaultAssistHoldTTL            = 20 * time.Minute
	MaxAssistHoldTTL                = 4 * time.Hour
	DefaultAssistMaxConcurrentHolds = 2
	MaxAssistConcurrentHolds        = 64
	DefaultAssistExecTimeout        = time.Minute
	MaxAssistExecTimeout            = 15 * time.Minute
	DefaultAssistExecOutputBytes    = int64(64 << 10)
	MaxAssistExecOutputBytes        = int64(4 << 20)
	DefaultAssistPatchMaxBytes      = int64(1 << 20)
	MaxAssistPatchMaxBytes          = int64(16 << 20)
	DefaultAssistPatchMaxFiles      = 64
	MaxAssistPatchMaxFiles          = 1024
)

// AssistConfig is the administrator policy for agent-assisted continuation.
// Every field is a Helm value rendered into a serve flag (assist.*).
type AssistConfig struct {
	// Enabled turns the feature on. Disabled, Build injects nothing even for
	// a run that asked, and the hold tools refuse.
	Enabled bool
	// HoldTTL bounds how long an idle parked retry waits for the agent
	// before the server sweep abandons it; a hold with agent activity ages
	// from its last action, up to AssistHoldMaxAgeFactor TTLs. Zero selects
	// DefaultAssistHoldTTL.
	HoldTTL time.Duration
	// MaxConcurrentHolds caps parked pods across the installation; the
	// newest hold above the cap is abandoned. Zero selects the default.
	MaxConcurrentHolds int
	// ExecTimeout, ExecOutputBytes bound one run_hold_exec command.
	ExecTimeout     time.Duration
	ExecOutputBytes int64
	// PatchMaxBytes, PatchMaxFiles bound one run_hold_patch diff.
	PatchMaxBytes int64
	PatchMaxFiles int
}

func (assist AssistConfig) withDefaults() AssistConfig {
	if assist.HoldTTL <= 0 {
		assist.HoldTTL = DefaultAssistHoldTTL
	}
	if assist.MaxConcurrentHolds <= 0 {
		assist.MaxConcurrentHolds = DefaultAssistMaxConcurrentHolds
	}
	if assist.ExecTimeout <= 0 {
		assist.ExecTimeout = DefaultAssistExecTimeout
	}
	if assist.ExecOutputBytes <= 0 {
		assist.ExecOutputBytes = DefaultAssistExecOutputBytes
	}
	if assist.PatchMaxBytes <= 0 {
		assist.PatchMaxBytes = DefaultAssistPatchMaxBytes
	}
	if assist.PatchMaxFiles <= 0 {
		assist.PatchMaxFiles = DefaultAssistPatchMaxFiles
	}
	return assist
}

// Validate bounds an administrator assist policy.
func (assist AssistConfig) Validate() error {
	if assist.HoldTTL < 0 || assist.HoldTTL > MaxAssistHoldTTL || assist.HoldTTL%time.Second != 0 {
		return fmt.Errorf("argojob: assist hold TTL must be whole seconds, 0 to %s", MaxAssistHoldTTL)
	}
	if assist.MaxConcurrentHolds < 0 || assist.MaxConcurrentHolds > MaxAssistConcurrentHolds {
		return fmt.Errorf("argojob: assist max concurrent holds must be 0 to %d", MaxAssistConcurrentHolds)
	}
	if assist.ExecTimeout < 0 || assist.ExecTimeout > MaxAssistExecTimeout {
		return fmt.Errorf("argojob: assist exec timeout must be 0 to %s", MaxAssistExecTimeout)
	}
	if assist.ExecOutputBytes < 0 || assist.ExecOutputBytes > MaxAssistExecOutputBytes {
		return fmt.Errorf("argojob: assist exec output bytes must be 0 to %d", MaxAssistExecOutputBytes)
	}
	if assist.PatchMaxBytes < 0 || assist.PatchMaxBytes > MaxAssistPatchMaxBytes {
		return fmt.Errorf("argojob: assist patch bytes must be 0 to %d", MaxAssistPatchMaxBytes)
	}
	if assist.PatchMaxFiles < 0 || assist.PatchMaxFiles > MaxAssistPatchMaxFiles {
		return fmt.Errorf("argojob: assist patch files must be 0 to %d", MaxAssistPatchMaxFiles)
	}
	return nil
}

// Effective returns the policy with defaults applied.
func (assist AssistConfig) Effective() AssistConfig { return assist.withDefaults() }

// assistActive reports whether this submission runs in assist mode: the
// administrator enabled it, the run asked, and it is branch-tier CI with no
// credentials anywhere in the workflow. Release, tag, promotion, schedule,
// plan and apply runs never hold: their steps publish or carry identity.
func assistActive(config Config, request Request, credentialed bool) bool {
	if !config.Assist.Enabled || !request.Assist || credentialed {
		return false
	}
	return request.Trigger == pipeline.TriggerCI && request.effectiveRunTrigger() == argoworkflow.RunTriggerBranch && request.ScheduleEntry == ""
}

// assistHoldEligible reports whether one leaf may park a failed retry. The
// leaf must be a single-main container or script template that Argo runs as
// a Pod, with no authored retryStrategy (its own retries keep their meaning),
// no credential chain, no step identity, no token, and no nonroot launcher
// (those lanes forbid exec and own their Pod patch).
func assistHoldEligible(t *wfv1.Template, workflowServiceAccount string, nonroot map[string]bool) bool {
	if t == nil || t.RetryStrategy != nil || t.DAG != nil || len(t.Steps) != 0 || t.Suspend != nil || t.Resource != nil || t.ContainerSet != nil || t.Data != nil || t.HTTP != nil || t.Plugin != nil {
		return false
	}
	if usageContainer(t) == nil || nonroot[t.Name] {
		return false
	}
	if argoworkflow.UsesCredentials(t) || t.Executor != nil {
		return false
	}
	// ForceIdentity stamps the workflow's pipeline identity on every leaf;
	// any other name is a per-step identity.
	if t.ServiceAccountName != "" && t.ServiceAccountName != workflowServiceAccount {
		return false
	}
	if t.AutomountServiceAccountToken != nil && *t.AutomountServiceAccountToken {
		return false
	}
	credentialMount := false
	templateContainers(t, func(c *corev1.Container) {
		for _, mount := range c.VolumeMounts {
			if mount.Name == ReleaseTokenVolumeName || mount.Name == SecretsVolumeName || mount.Name == releaseWIFVolume || mount.MountPath == ReleaseTokenMountPath {
				credentialMount = true
			}
		}
	})
	return !credentialMount
}

// markAssistHolds runs early, before executor tokens and placement read the
// workflow deadline: it stamps hold-eligible leaves, extends the workflow
// deadline by the hold allowance (AssistHoldMaxAgeFactor TTLs) so a parked step cannot be cut short by the
// run's own budget, and records assist mode on the Workflow. The hold
// annotations are server-owned: authored copies are removed first.
func markAssistHolds(workflow *wfv1.Workflow, config Config, request Request, credentialed bool) error {
	delete(workflow.Annotations, AssistWorkflowAnnotation)
	walkTemplates(workflow, func(t *wfv1.Template) { delete(t.Metadata.Annotations, AssistHoldAnnotation) })
	if !assistActive(config, request, credentialed) {
		return nil
	}
	// A default retryStrategy would be replaced on every eligible leaf by
	// the injected one; rather than change the document's retry meaning,
	// such a document never holds.
	if workflow.Spec.TemplateDefaults != nil && workflow.Spec.TemplateDefaults.RetryStrategy != nil {
		return nil
	}
	names, err := argoworkflow.DeclaredNonrootTemplates(workflow)
	if err != nil {
		return err
	}
	nonroot := make(map[string]bool, len(names))
	for _, name := range names {
		nonroot[name] = true
	}
	assist := config.Assist.withDefaults()
	ttl := int64(assist.HoldTTL / time.Second)
	workflowDeadline := int64(0)
	if workflow.Spec.ActiveDeadlineSeconds != nil {
		workflowDeadline = *workflow.Spec.ActiveDeadlineSeconds
	}
	eligible := 0
	walkTemplates(workflow, func(t *wfv1.Template) {
		if !assistHoldEligible(t, workflow.Spec.ServiceAccountName, nonroot) {
			return
		}
		// The held attempt keeps the step's whole (#1499-floored) budget for
		// the command after release, plus the hold itself. A leaf without its
		// own deadline inherits the (floored) templateDefaults deadline, which
		// Argo merges into every leaf before it creates the Pod
		// (operator.go mergedTemplateDefaultsInto); without that too it is
		// bounded by the workflow budget, so that is its step budget here.
		budget := workflowDeadline
		if deadline := leafDeadline(t, workflow.Spec.TemplateDefaults); deadline != nil {
			declared, err := strconv.ParseInt(deadline.String(), 10, 64)
			if err != nil || declared <= 0 {
				return // non-literal (Argo substitutes it later): never guess a hold deadline.
			}
			budget = declared
		}
		if budget <= 0 || budget > 1<<40 {
			return
		}
		if t.Metadata.Annotations == nil {
			t.Metadata.Annotations = map[string]string{}
		}
		t.Metadata.Annotations[AssistHoldAnnotation] = strconv.FormatInt(budget+assistHoldAllowance(ttl), 10)
		eligible++
		if request.binaryDelivered() && request.SourceVolume.ClaimName != "" {
			// run_hold_patch applies files through this server's own static
			// executable, not through whatever shell the image may lack. The
			// mount is the read-only one measured steps already receive;
			// wrapStepUsage still wraps only leaves above its memory floor.
			main := usageContainer(t)
			mounts := main.VolumeMounts[:0:0]
			for _, m := range main.VolumeMounts {
				if m.MountPath != usageBinMountPath {
					mounts = append(mounts, m)
				}
			}
			main.VolumeMounts = append(mounts, corev1.VolumeMount{Name: SourceVolumeName, MountPath: usageBinMountPath, SubPath: request.SourceVolume.BinarySubPath, ReadOnly: true})
		}
	})
	if eligible == 0 {
		return nil
	}
	if workflow.Spec.ActiveDeadlineSeconds != nil && *workflow.Spec.ActiveDeadlineSeconds > 0 {
		extended := *workflow.Spec.ActiveDeadlineSeconds + assistHoldAllowance(ttl)
		workflow.Spec.ActiveDeadlineSeconds = &extended
	}
	if workflow.Annotations == nil {
		workflow.Annotations = map[string]string{}
	}
	workflow.Annotations[AssistWorkflowAnnotation] = strconv.FormatInt(ttl, 10)
	return nil
}

// leafDeadline is the activeDeadlineSeconds the kubelet will enforce on a
// leaf's Pod before the workflow budget is considered: the leaf's own, or
// the document's templateDefaults value.
func leafDeadline(t *wfv1.Template, defaults *wfv1.Template) *intstr.IntOrString {
	if t.ActiveDeadlineSeconds != nil {
		return t.ActiveDeadlineSeconds
	}
	if defaults != nil {
		return defaults.ActiveDeadlineSeconds
	}
	return nil
}

// injectAssistHolds runs last among the Pod patch writers: it turns each
// marked leaf's plain server patch into Argo's conditional expression form.
// Every earlier pass merges JSON into t.PodSpecPatch; an expression there
// would break them, so this pass must stay after wrapStepUsage.
func injectAssistHolds(workflow *wfv1.Workflow) error {
	rawTTL, assisted := workflow.Annotations[AssistWorkflowAnnotation]
	if !assisted {
		return nil
	}
	ttl, err := strconv.ParseInt(rawTTL, 10, 64)
	if err != nil || ttl <= 0 {
		return fmt.Errorf("argojob: %s on the workflow is not a hold TTL", AssistWorkflowAnnotation)
	}
	var failure error
	walkTemplates(workflow, func(t *wfv1.Template) {
		raw, marked := t.Metadata.Annotations[AssistHoldAnnotation]
		if failure != nil || !marked {
			return
		}
		deadline, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || deadline <= assistHoldAllowance(ttl) {
			failure = fmt.Errorf("argojob: %s on %q is not a hold deadline above the hold allowance", AssistHoldAnnotation, t.Name)
			return
		}
		// markAssistHolds wrote the floored step budget plus the hold
		// allowance (AssistHoldMaxAgeFactor TTLs); the budget is the bound a
		// failed command must have finished under.
		budget := deadline - assistHoldAllowance(ttl)
		main := usageContainer(t)
		if main == nil {
			failure = fmt.Errorf("argojob: hold-eligible template %q lost its main container", t.Name)
			return
		}
		base := map[string]any{}
		if t.PodSpecPatch != "" {
			if err := json.Unmarshal([]byte(t.PodSpecPatch), &base); err != nil {
				failure = fmt.Errorf("argojob: template %q Pod patch is not plain JSON: %w", t.Name, err)
				return
			}
		}
		held, err := assistHoldPatch(base, main, deadline)
		if err != nil {
			failure = err
			return
		}
		patch, err := assistConditionalPatch(base, held)
		if err != nil {
			failure = fmt.Errorf("argojob: template %q: %w", t.Name, err)
			return
		}
		t.PodSpecPatch = patch
		limit := intstr.FromInt32(1)
		t.RetryStrategy = &wfv1.RetryStrategy{Limit: &limit, RetryPolicy: wfv1.RetryPolicyOnFailure, Expression: assistRetryExpression(budget)}
	})
	return failure
}

// assistHoldPatch is the Pod patch of a held attempt: the base server patch
// plus the pause, the wider kubelet deadline, a writable source mount, a
// read-only shared cache and cache variables moved into private scratch.
func assistHoldPatch(base map[string]any, main *corev1.Container, deadline int64) (map[string]any, error) {
	encoded, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	held := map[string]any{}
	if err := json.Unmarshal(encoded, &held); err != nil {
		return nil, err
	}
	held["activeDeadlineSeconds"] = deadline
	env := []map[string]any{{"name": assistPauseEnv, "value": "true"}, {"name": AssistHoldEnv, "value": "1"}}
	for _, variable := range main.Env {
		switch {
		case variable.ValueFrom != nil:
		case variable.Value == CacheMountPath:
			env = append(env, map[string]any{"name": variable.Name, "value": AssistScratchCachePath})
		case strings.HasPrefix(variable.Value, CacheMountPath+"/"):
			env = append(env, map[string]any{"name": variable.Name, "value": AssistScratchCachePath + "/" + path.Base(variable.Value)})
		}
	}
	var mounts []map[string]any
	for _, mount := range main.VolumeMounts {
		switch {
		case mount.Name == SourceVolumeName && mount.MountPath == SourceMountPath:
			mounts = append(mounts, map[string]any{"name": mount.Name, "mountPath": mount.MountPath, "readOnly": false})
		case mount.Name == CacheVolumeName && mount.MountPath == CacheMountPath:
			mounts = append(mounts, map[string]any{"name": mount.Name, "mountPath": mount.MountPath, "readOnly": true})
		}
	}
	container := map[string]any{"name": mainContainerName, "env": env}
	if len(mounts) > 0 {
		container["volumeMounts"] = mounts
	}
	containers, _ := held["containers"].([]any)
	replaced := false
	for index, entry := range containers {
		existing, _ := entry.(map[string]any)
		if existing["name"] == mainContainerName {
			for key, value := range container {
				existing[key] = value
			}
			containers[index] = existing
			replaced = true
		}
	}
	if !replaced {
		containers = append(containers, container)
	}
	held["containers"] = containers
	if hasUsageBinary(main) {
		// run_hold_patch runs this server's executable inside the held pod.
		selectors, _ := held["nodeSelector"].(map[string]any)
		if selectors == nil {
			selectors = map[string]any{}
		}
		selectors[corev1.LabelArchStable] = runtime.GOARCH
		held["nodeSelector"] = selectors
	}
	return held, nil
}

func hasUsageBinary(main *corev1.Container) bool {
	return slices.ContainsFunc(main.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == SourceVolumeName && m.MountPath == usageBinMountPath && m.ReadOnly
	})
}

// assistConditionalPatch renders both branches into one Argo expression
// template. The controller substitutes it with the retry-local variables
// right before it creates each attempt's Pod (operator.go, SubstituteParams
// with retries/lastRetry.*), after the template-level pause check that would
// otherwise drop the Pod deadline. Each branch is a single-quoted expr
// string literal holding compact JSON.
func assistConditionalPatch(base, held map[string]any) (string, error) {
	baseLiteral, err := exprJSONLiteral(base)
	if err != nil {
		return "", err
	}
	heldLiteral, err := exprJSONLiteral(held)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("{{= %s ? '%s' : '%s' }}", assistPatchCondition, heldLiteral, baseLiteral), nil
}

// exprJSONLiteral encodes a value as JSON that can sit inside a single-quoted
// expr string within an Argo `{{= }}` template. expr unescapes string
// literals like strconv (parser/lexer/utils.go), so backslashes and single
// quotes are escaped; HTML escaping is off because the template round trip
// would re-escape the backslash. Argo's template scanner ends the expression
// at the first `}}`, so structural closing braces are separated by a space,
// which JSON permits, and a `}}` or `{{` inside a string is refused.
func exprJSONLiteral(value any) (string, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	compact := strings.TrimSuffix(buffer.String(), "\n")
	var out strings.Builder
	inString, escaped := false, false
	for index := 0; index < len(compact); index++ {
		character := compact[index]
		if inString {
			if index+1 < len(compact) && ((character == '{' && compact[index+1] == '{') || (character == '}' && compact[index+1] == '}')) {
				return "", errors.New("hold patch string contains a template delimiter")
			}
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			switch character {
			case '\\':
				out.WriteString("\\\\")
			case '\'':
				out.WriteString("\\'")
			default:
				out.WriteByte(character)
			}
			continue
		}
		if character == '"' {
			inString = true
		}
		out.WriteByte(character)
		if character == '}' && index+1 < len(compact) && compact[index+1] == '}' {
			out.WriteByte(' ')
		}
	}
	return out.String(), nil
}

// AssistHoldPod reports whether a Pod is a parked assist attempt: it carries
// the server-owned hold annotation and the emissary pause in its main
// container, which only the held branch of the patch ever adds.
func AssistHoldPod(pod *corev1.Pod) bool {
	if pod == nil || pod.Annotations[AssistHoldAnnotation] == "" {
		return false
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != mainContainerName {
			continue
		}
		for _, variable := range c.Env {
			if variable.Name == assistPauseEnv && variable.Value == "true" {
				return true
			}
		}
	}
	return false
}
