package model

import (
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
	// or an exec before release. A continue without either is still not
	// evidence (see docs/assisted-continuation.md), so the run is assisted
	// either way; the flag only shapes the reason line.
	Assisted bool `json:"assisted,omitempty"`
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
// existing status explains itself. Example:
// "assisted: step test continued after patch sha256:ab12… by uplink-a".
func (record RunAssistRecord) Reason() string {
	var parts []string
	for _, event := range record.Events {
		if event.Kind != RunAssistContinue {
			continue
		}
		detail := "continued without changes"
		if sha := record.lastPatch(event.Step); sha != "" {
			detail = "continued after patch sha256:" + shortDigest(sha)
		} else if record.Touched(event.Step) {
			detail = "continued after exec"
		}
		parts = append(parts, fmt.Sprintf("step %s %s by %s", event.Step, detail, event.Actor))
	}
	if len(parts) == 0 {
		return ""
	}
	return "assisted: " + strings.Join(parts, "; ") + "; not evidence"
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
	// Authorize runs inside the writer transaction against the current run.
	Authorize func(Run) error
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
type AssistPolicy struct {
	Enabled            bool          `json:"enabled"`
	HoldTTL            time.Duration `json:"hold_ttl"`
	MaxConcurrentHolds int           `json:"max_concurrent_holds"`
	ExecTimeout        time.Duration `json:"exec_timeout"`
	ExecOutputBytes    int64         `json:"exec_output_bytes"`
	PatchMaxBytes      int64         `json:"patch_max_bytes"`
	PatchMaxFiles      int           `json:"patch_max_files"`
}
