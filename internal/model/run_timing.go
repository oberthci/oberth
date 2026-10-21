package model

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

const TimingVersion = 1
const MaxTimingTasks = 2048
const MaxStepTimingBytes = 16 << 10

// A durable run ID is an execution attempt. Re-fires have a new ID, even when
// their admission sequence is inherited; a resumed Workflow keeps its ID.
type RunTiming struct {
	Version             int               `json:"version"`
	RunID               string            `json:"run_id"`
	Unavailable         string            `json:"unavailable,omitempty"`
	QueuedAt            time.Time         `json:"queued_at"`
	ClaimedAt           *time.Time        `json:"claimed_at,omitempty"`
	CheckoutReadyAt     *time.Time        `json:"checkout_ready_at,omitempty"`
	SourceSeedStartedAt *time.Time        `json:"source_seed_started_at,omitempty"`
	SourceReadyAt       *time.Time        `json:"source_ready_at,omitempty"`
	WorkflowCreatedAt   *time.Time        `json:"workflow_created_at,omitempty"`
	TerminalAt          *time.Time        `json:"terminal_at,omitempty"`
	Workflow            *WorkflowTiming   `json:"workflow,omitempty"`
	Admissions          []AdmissionTiming `json:"admissions"`
	Steps               []StepTiming      `json:"steps"`
}

type AdmissionTiming struct {
	Episode     int        `json:"episode"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	GrantedAt   *time.Time `json:"granted_at,omitempty"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
}

// Tasks contain admitted expanded identities and dependency names, never
// commands, arguments, conditions, environment or Kubernetes object dumps.
type WorkflowTiming struct {
	RunID            string       `json:"run_id"`
	Namespace        string       `json:"namespace"`
	Name             string       `json:"name"`
	UID              string       `json:"uid"`
	SpecIdentity     string       `json:"spec_identity"`
	CreatedAt        time.Time    `json:"created_at"`
	Tasks            []TimingTask `json:"tasks"`
	GraphUnavailable string       `json:"graph_unavailable,omitempty"`
}

type TimingTask struct {
	ID           string   `json:"id"`
	Template     string   `json:"template"`
	Dependencies []string `json:"dependencies"`
	Readiness    string   `json:"readiness"`
}

type ContainerTiming struct {
	Name         string    `json:"name"`
	ContainerID  string    `json:"container_id"`
	RestartCount int32     `json:"restart_count"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

type StepTiming struct {
	Version           int               `json:"version"`
	Available         bool              `json:"available"`
	Unavailable       string            `json:"unavailable,omitempty"`
	RunID             string            `json:"run_id"`
	Namespace         string            `json:"namespace"`
	WorkflowName      string            `json:"workflow_name"`
	WorkflowUID       string            `json:"workflow_uid"`
	NodeID            string            `json:"node_id"`
	PodName           string            `json:"pod_name,omitempty"`
	PodUID            string            `json:"pod_uid,omitempty"`
	Burn              string            `json:"burn"`
	Step              string            `json:"step"`
	Ordinal           int               `json:"ordinal"`
	DependencyReadyAt *time.Time        `json:"dependency_ready_at,omitempty"`
	PodCreatedAt      *time.Time        `json:"pod_created_at,omitempty"`
	ScheduledAt       *time.Time        `json:"scheduled_at,omitempty"`
	Init              []ContainerTiming `json:"init"`
	Main              *ContainerTiming  `json:"main,omitempty"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	ObservedAt        time.Time         `json:"observed_at"`
	// Missing timestamps are unknown, not zero-duration phases.
	Missing         []string `json:"missing"`
	ScriptTiming    string   `json:"script_timing"`
	ImagePullTiming string   `json:"image_pull_timing"`
}

var timingIdentity = regexp.MustCompile(`^[0-9a-f]{64}$`)
var timingName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)

func (value WorkflowTiming) Validate() error {
	if !diagnosticRun.MatchString(value.RunID) || !timingName.MatchString(value.Namespace) || !timingName.MatchString(value.Name) || !diagnosticUID.MatchString(value.UID) || !timingIdentity.MatchString(value.SpecIdentity) || value.CreatedAt.IsZero() || len(value.Tasks) > MaxTimingTasks {
		return errors.New("invalid Workflow timing binding")
	}
	if value.GraphUnavailable != "" && value.GraphUnavailable != "unsupported-graph" && value.GraphUnavailable != "graph-bound" {
		return errors.New("invalid timing graph availability")
	}
	seen := map[string]bool{}
	for _, task := range value.Tasks {
		if !timingName.MatchString(task.ID) || !timingName.MatchString(task.Template) || seen[task.ID] || len(task.Dependencies) > MaxTimingTasks {
			return errors.New("invalid timing task")
		}
		seen[task.ID] = true
		if task.Readiness != "all" && task.Readiness != "conditional" && task.Readiness != "dynamic" && task.Readiness != "expression-unavailable" {
			return errors.New("invalid timing readiness")
		}
		for _, dependency := range task.Dependencies {
			if !timingName.MatchString(dependency) {
				return errors.New("invalid timing dependency")
			}
		}
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > 256<<10 {
		return errors.New("timing graph exceeds bound")
	}
	return nil
}

func (value StepTiming) Validate() error {
	fail := errors.New("invalid step timing")
	if value.Version != TimingVersion || !diagnosticRun.MatchString(value.RunID) || !timingName.MatchString(value.Namespace) || !timingName.MatchString(value.WorkflowName) || !diagnosticUID.MatchString(value.WorkflowUID) || !timingName.MatchString(value.NodeID) || !timingName.MatchString(value.Burn) || !timingName.MatchString(value.Step) || value.Ordinal < 0 || value.ObservedAt.IsZero() || len(value.Init) > 16 || value.ScriptTiming != "mixed" || value.ImagePullTiming != "not-observed" {
		return fail
	}
	if value.PodName != "" && !timingName.MatchString(value.PodName) {
		return fail
	}
	if value.PodUID != "" && !diagnosticUID.MatchString(value.PodUID) {
		return fail
	}
	if value.Available {
		if value.Unavailable != "" || value.Main == nil || value.Main.Name != "main" || value.PodName == "" || value.PodUID == "" {
			return fail
		}
	} else {
		switch value.Unavailable {
		case "not-executed", "ambiguous-attempt", "binding-mismatch", "observation-unavailable", "snapshot-incomplete", "unsupported-shape", "termination-unavailable", "invalid-termination":
		default:
			return fail
		}
		if value.Main != nil {
			return fail
		}
	}
	for _, at := range []*time.Time{value.DependencyReadyAt, value.PodCreatedAt, value.ScheduledAt, value.CompletedAt} {
		if at != nil && (at.IsZero() || at.After(value.ObservedAt)) {
			return fail
		}
	}
	containers := append([]ContainerTiming{}, value.Init...)
	if value.Main != nil {
		containers = append(containers, *value.Main)
	}
	seen := map[string]bool{}
	for _, container := range containers {
		if !timingName.MatchString(container.Name) || seen[container.Name] || !diagnosticCID.MatchString(container.ContainerID) || container.RestartCount < 0 || container.StartedAt.IsZero() || container.FinishedAt.Before(container.StartedAt) || container.FinishedAt.After(value.ObservedAt) {
			return fail
		}
		seen[container.Name] = true
	}
	for _, missing := range value.Missing {
		switch missing {
		case "dependency-ready", "pod-created", "scheduled", "init", "main", "completed":
		default:
			return fail
		}
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > MaxStepTimingBytes {
		return fail
	}
	return nil
}
