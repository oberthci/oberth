package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Agent-assisted continuation (retry hold). A branch CI run that opted in
// may park a failed step's retry so its agent can inspect, patch and release
// it. Every action is appended to the run's assist record; a continue marks
// the run assisted, which removes it from every green-evidence path.
const (
	RunAssistExec     = "exec"
	RunAssistPatch    = "patch"
	RunAssistContinue = "continue"
	RunAssistAbandon  = "abandon"

	// MaximumRunAssistRecordBytes bounds the stored JSON record.
	MaximumRunAssistRecordBytes = 256 << 10
)

// RunAssistEvent is one audited hold action.
type RunAssistEvent struct {
	At    time.Time `json:"at"`
	Actor string    `json:"actor"`
	Step  string    `json:"step"`
	Pod   string    `json:"pod"`
	Kind  string    `json:"kind"`
	// PatchSHA256 and PatchBytes describe an applied unified diff.
	PatchSHA256 string   `json:"patch_sha256,omitempty"`
	PatchBytes  int64    `json:"patch_bytes,omitempty"`
	Files       []string `json:"files,omitempty"`
	// Argv and ExitCode describe a bounded exec.
	Argv     []string `json:"argv,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	// Assisted is recorded on a continue: whether the held step saw a patch
	// or an exec before release. The run itself is assisted from the first
	// exec, patch or continue (see docs/assisted-continuation.md); the flag
	// only shapes the reason line.
	Assisted bool `json:"assisted,omitempty"`
	// AbandonReason, HeldSince, FailedAttempt and FailedExitCode describe a
	// hold the server's sweep abandoned (actor "oberth"): reason
	// AssistAbandonTTL or AssistAbandonCap, when the hold parked, and the
	// attempt whose failure created it. At is the abandon time.
	AbandonReason  string     `json:"abandon_reason,omitempty"`
	HeldSince      *time.Time `json:"held_since,omitempty"`
	FailedAttempt  int        `json:"failed_attempt,omitempty"`
	FailedExitCode *int       `json:"failed_exit_code,omitempty"`
	// Eviction is the kubelet's message for a held or continued attempt
	// the node evicted (AbandonReason AssistAbandonEvicted), bounded.
	Eviction string `json:"eviction,omitempty"`
}

// Sweep abandon reasons. AssistAbandonEvicted is not an abandon the sweep
// chose: it records that the kubelet evicted a held or continued attempt,
// so the reason line names the eviction instead of a plain step failure.
const (
	AssistAbandonTTL     = "ttl"
	AssistAbandonCap     = "cap"
	AssistAbandonEvicted = "evicted"
)

// MaximumAssistEvictionLength bounds the recorded kubelet message.
const MaximumAssistEvictionLength = 200

// AssistActivityWindow is how long after an agent's last exec, patch or
// continue a hold counts as being worked: the cap never abandons such a hold.
const AssistActivityWindow = 5 * time.Minute

// AssistPodActivity is what the active runs' assist records say about one
// held Pod: released (continued or abandoned) holds are running work or
// gone; LastAction is the latest exec, patch or continue on it.
type AssistPodActivity struct {
	Released   bool
	LastAction time.Time
	// Evicted: the record already holds the Pod's eviction.
	Evicted bool
}

// AssistActivityRecheck re-reads one held Pod's activity from its run's
// current assist record. The sweep calls it for each chosen victim right
// before the delete, so an action that landed after the sweep's snapshot
// (a continue, or an exec or patch inside the protection window) is seen.
type AssistActivityRecheck func(ctx context.Context, runID, pod string) (AssistPodActivity, error)

// PodActivities is the per-Pod activity this record shows: a continue or
// abandon releases the Pod; LastAction is its latest exec, patch or
// continue.
func (record RunAssistRecord) PodActivities() map[string]AssistPodActivity {
	pods := map[string]AssistPodActivity{}
	for _, event := range record.Events {
		if event.Pod == "" {
			continue
		}
		activity := pods[event.Pod]
		if event.Kind == RunAssistContinue || event.Kind == RunAssistAbandon {
			activity.Released = true
		}
		if event.Kind == RunAssistAbandon && event.AbandonReason == AssistAbandonEvicted {
			activity.Evicted = true
		}
		if event.Kind != RunAssistAbandon && event.At.After(activity.LastAction) {
			activity.LastAction = event.At
		}
		pods[event.Pod] = activity
	}
	return pods
}

// AssistAbandon is one hold the server sweep abandoned.
type AssistAbandon struct {
	RunID          string
	Pod            string
	Step           string
	Reason         string
	HeldSince      time.Time
	AbandonedAt    time.Time
	FailedAttempt  int
	FailedExitCode *int
	Eviction       string
}

// Event is the assist record entry for a sweep abandon.
func (abandon AssistAbandon) Event() RunAssistEvent {
	held := abandon.HeldSince.UTC()
	eviction := []rune(strings.Join(strings.Fields(abandon.Eviction), " "))
	if len(eviction) > MaximumAssistEvictionLength {
		eviction = append(eviction[:MaximumAssistEvictionLength-1], '…')
	}
	return RunAssistEvent{At: abandon.AbandonedAt.UTC(), Actor: "oberth", Step: abandon.Step, Pod: abandon.Pod, Kind: RunAssistAbandon,
		AbandonReason: abandon.Reason, HeldSince: &held, FailedAttempt: abandon.FailedAttempt, FailedExitCode: abandon.FailedExitCode, Eviction: string(eviction)}
}

// RunAssistRecord is the JSON stored in runs.assist_record.
type RunAssistRecord struct {
	Events []RunAssistEvent `json:"events"`
}

// Continued reports whether any held step was released.
func (record RunAssistRecord) Continued() bool {
	for _, event := range record.Events {
		if event.Kind == RunAssistContinue {
			return true
		}
	}
	return false
}

// SweepAbandoned reports whether the server sweep abandoned a hold of step.
func (record RunAssistRecord) SweepAbandoned(step string) bool {
	for _, event := range record.Events {
		if event.Step == step && event.Kind == RunAssistAbandon && event.AbandonReason != "" {
			return true
		}
	}
	return false
}

// Touched reports whether the named step saw an exec or a patch.
func (record RunAssistRecord) Touched(step string) bool {
	for _, event := range record.Events {
		if event.Step == step && (event.Kind == RunAssistExec || event.Kind == RunAssistPatch) {
			return true
		}
	}
	return false
}

// Reason is the reason-line fragment for an assisted run: no new status, the
// existing status explains itself. One clause per touched step, e.g.
// "assisted: step test continued after patch sha256:ab12… by uplink-a; not
// evidence"; a step that was only inspected or patched reads "step test
// patched sha256:… by uplink-a" or "step test inspected by uplink-a".
//
// A hold the server sweep abandoned reads "step test failed at attempt 0
// (exit 1); hold abandoned unserviced after 20m0s (ttl)", without
// "unserviced" when its Pod saw an exec or patch first. A record with only
// sweep abandons (no agent exec, patch, continue or abandon) belongs to a run
// that was never assisted: its attempt-0 failure is real, so the line is the
// sweep clauses alone, without "assisted:" or "not evidence".
func (record RunAssistRecord) Reason() string {
	var order []string
	actors := map[string]string{}
	var swept []string
	for _, event := range record.Events {
		if event.Kind == RunAssistAbandon {
			if event.AbandonReason != "" {
				swept = append(swept, record.sweepClause(event))
			}
			continue
		}
		if _, seen := actors[event.Step]; !seen {
			order = append(order, event.Step)
		}
		actors[event.Step] = event.Actor
	}
	var parts []string
	for _, step := range order {
		sha, continued, touched := record.lastPatch(step), false, record.Touched(step)
		for _, event := range record.Events {
			if event.Step == step && event.Kind == RunAssistContinue {
				continued = true
			}
		}
		detail := ""
		switch {
		case continued && sha != "":
			detail = "continued after patch sha256:" + shortDigest(sha)
		case continued && touched:
			detail = "continued after exec"
		case continued:
			detail = "continued without changes"
		case sha != "":
			detail = "patched sha256:" + shortDigest(sha)
		default:
			detail = "inspected"
		}
		parts = append(parts, fmt.Sprintf("step %s %s by %s", step, detail, actors[step]))
	}
	if len(order) == 0 {
		return strings.Join(swept, "; ")
	}
	parts = append(parts, swept...)
	return "assisted: " + strings.Join(parts, "; ") + "; not evidence"
}

func (record RunAssistRecord) sweepClause(abandon RunAssistEvent) string {
	if abandon.AbandonReason == AssistAbandonEvicted {
		return record.evictionClause(abandon)
	}
	exit := "exit unknown"
	if abandon.FailedExitCode != nil {
		exit = fmt.Sprintf("exit %d", *abandon.FailedExitCode)
	}
	held := "unknown time"
	if abandon.HeldSince != nil && !abandon.At.Before(*abandon.HeldSince) {
		held = abandon.At.Sub(*abandon.HeldSince).Round(time.Second).String()
	}
	serviced := "unserviced "
	for _, event := range record.Events {
		if event.Pod == abandon.Pod && (event.Kind == RunAssistExec || event.Kind == RunAssistPatch) {
			serviced = ""
		}
	}
	return fmt.Sprintf("step %s failed at attempt %d (%s); hold abandoned %safter %s (%s)", abandon.Step, abandon.FailedAttempt, exit, serviced, held, abandon.AbandonReason)
}

// evictionClause reads "step test evicted after continue: ephemeral-storage
// (<kubelet message>)", or "evicted while held" when nobody released it.
func (record RunAssistRecord) evictionClause(eviction RunAssistEvent) string {
	when := "while held"
	for _, event := range record.Events {
		if event.Pod == eviction.Pod && event.Kind == RunAssistContinue {
			when = "after continue"
		}
	}
	text := "step " + eviction.Step + " evicted " + when + ": " + EvictionResource(eviction.Eviction)
	if eviction.Eviction != "" {
		text += " (" + eviction.Eviction + ")"
	}
	return text
}

// EvictionResource names the resource a kubelet eviction message is about.
func EvictionResource(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "ephemeral"):
		return "ephemeral-storage"
	case strings.Contains(lower, "memory"):
		return "memory"
	case strings.Contains(lower, "pid"):
		return "pids"
	case strings.Contains(lower, "disk") || strings.Contains(lower, "storage"):
		return "disk"
	}
	return "node pressure"
}

func (record RunAssistRecord) lastPatch(step string) string {
	sha := ""
	for _, event := range record.Events {
		if event.Step == step && event.Kind == RunAssistPatch {
			sha = event.PatchSHA256
		}
	}
	return sha
}

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// RunAssistRequest appends one hold action to a run.
type RunAssistRequest struct {
	RunID string
	Event RunAssistEvent
	// Authorize runs inside the writer transaction against the current run
	// and the name of the upstream its repository belongs to now.
	Authorize func(run Run, upstream string) error
}

// AssistHold is one parked retry attempt as the engine sees it.
type AssistHold struct {
	Pod     string `json:"pod"`
	Step    string `json:"step"`
	Node    string `json:"node"`
	Attempt int    `json:"attempt"`
	// HeldSince is when the paused main container started; Deadline is when
	// the kubelet fails the Pod if nobody acts.
	HeldSince time.Time `json:"held_since"`
	Deadline  time.Time `json:"deadline"`
	// PatchCapable reports that the server's helper executable is mounted,
	// so run_hold_patch can apply files; exec works either way.
	PatchCapable bool `json:"patch_capable"`
	// SourcePath is the writable source tree inside the held container.
	SourcePath string `json:"source_path"`
	// Released is filled from the run's assist record: the hold was
	// continued or abandoned already.
	Released bool `json:"released,omitempty"`
	// AbandonAt is when the server's TTL sweep abandons this hold if nobody
	// acts (AssistHoldExpiry), filled by the service for unreleased holds.
	// It comes before Deadline, the kubelet's own bound.
	AbandonAt *time.Time `json:"abandon_at,omitempty"`
}

// AssistHoldMaxAgeFactor caps how far actions extend a hold: at most this
// many hold TTLs after held_since.
const AssistHoldMaxAgeFactor = 2

// AssistHoldExpiry is when the TTL abandons a hold: held_since + ttl for an
// idle hold, last action + ttl for a worked one, never later than
// AssistHoldMaxAgeFactor TTLs after held_since.
func AssistHoldExpiry(hold AssistHold, activity AssistPodActivity, ttl time.Duration) time.Time {
	start := hold.HeldSince
	if activity.LastAction.After(start) {
		start = activity.LastAction
	}
	limit := hold.HeldSince.Add(AssistHoldMaxAgeFactor * ttl)
	if expiry := start.Add(ttl); expiry.Before(limit) {
		return expiry
	}
	return limit
}

// AssistExecRequest runs one bounded argv in a held Pod's main container.
type AssistExecRequest struct {
	Pod         string
	Argv        []string
	Stdin       []byte
	Timeout     time.Duration
	OutputLimit int64
}

// AssistExecResult is the bounded outcome.
type AssistExecResult struct {
	ExitCode    int           `json:"exit_code"`
	Stdout      string        `json:"stdout"`
	Stderr      string        `json:"stderr"`
	Truncated   bool          `json:"truncated,omitempty"`
	StdoutBytes int64         `json:"stdout_bytes"`
	StderrBytes int64         `json:"stderr_bytes"`
	Duration    time.Duration `json:"duration"`
	TimedOut    bool          `json:"timed_out,omitempty"`
}

// AssistPolicy is the administrator policy the tools report and enforce.
// Its JSON form spells durations as seconds (hold_ttl_s, exec_timeout_s) and
// as Go duration text (hold_ttl "20m0s"), never as nanoseconds.
type AssistPolicy struct {
	Enabled            bool          `json:"enabled"`
	HoldTTL            time.Duration `json:"hold_ttl"`
	MaxConcurrentHolds int           `json:"max_concurrent_holds"`
	ExecTimeout        time.Duration `json:"exec_timeout"`
	ExecOutputBytes    int64         `json:"exec_output_bytes"`
	PatchMaxBytes      int64         `json:"patch_max_bytes"`
	PatchMaxFiles      int           `json:"patch_max_files"`
	// ScratchBytes is the ephemeral storage a held attempt's main
	// container gets on top of its own, for its private cold caches.
	ScratchBytes int64 `json:"scratch_bytes,omitempty"`
}

// MarshalJSON reports the durations an agent plans against in seconds and
// as readable text instead of raw nanoseconds.
func (policy AssistPolicy) MarshalJSON() ([]byte, error) {
	type wire AssistPolicy
	return json.Marshal(struct {
		wire
		HoldTTL      string `json:"hold_ttl"`
		HoldTTLS     int64  `json:"hold_ttl_s"`
		ExecTimeout  string `json:"exec_timeout"`
		ExecTimeoutS int64  `json:"exec_timeout_s"`
	}{wire: wire(policy), HoldTTL: policy.HoldTTL.String(), HoldTTLS: int64(policy.HoldTTL / time.Second),
		ExecTimeout: policy.ExecTimeout.String(), ExecTimeoutS: int64(policy.ExecTimeout / time.Second)})
}
