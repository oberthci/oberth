// Package measured reports server-captured resource history. Suggestions are
// advice for a reviewed git change, never an execution or admission policy.
package measured

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/model"
)

const DefaultRuns = 30
const MaxRuns = 100
const MaxRows = 10000
const MinimumSamples = 5

// MinimumMemoryPolls avoids treating a short, sparsely observed attempt's
// sampled memory as its peak. A bound step-reported cgroup peak does not need
// this polling coverage, but remains advisory and explicitly self-reported.
const MinimumMemoryPolls int64 = 4

// RequestMarginPercent covers normal variation above p95 without reserving a maximum for
// every run. Limits use the maximum, never the median, plus 50% to allow bursts
// missed between 15-second polls. These are suggestions, not safety proofs.
const RequestMarginPercent = 25
const LimitMarginPercent = 50
const DeadlineMarginPercent = 50
const DeadlineStartupSeconds = 60

type Observation struct {
	StepMemoryPeakBytes *int64
	DeadlineKilled      bool
	Ambiguous           bool
	Usage               model.StepResourceUsage
	Template            string
	Owner               string
	CPULimitNanoCores   *int64
	MemoryLimitBytes    *int64
	EphemeralLimitBytes *int64
}
type Declaration struct {
	ResolvedTemplate      string                       `json:"-"`
	Stage                 string                       `json:"burn"`
	Template              string                       `json:"template"`
	OwnerRepository       string                       `json:"owner_repository"`
	SHA                   string                       `json:"sha"`
	Resources             map[string]map[string]string `json:"resources"`
	ActiveDeadlineSeconds *int64                       `json:"activeDeadlineSeconds,omitempty"`
}
type Suggested struct {
	CPUMilli              int64 `json:"cpu_milli"`
	MemoryRequestBytes    int64 `json:"memory_request_bytes"`
	MemoryLimitBytes      int64 `json:"memory_limit_bytes"`
	EphemeralRequestBytes int64 `json:"ephemeral_request_bytes"`
	EphemeralLimitBytes   int64 `json:"ephemeral_limit_bytes"`
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds"`
}

func (s Suggested) Resources() map[string]map[string]string {
	quantity := func(n int64) string { return resource.NewQuantity(n, resource.BinarySI).String() }
	return map[string]map[string]string{
		"requests": {"cpu": fmt.Sprintf("%dm", s.CPUMilli), "memory": quantity(s.MemoryRequestBytes), "ephemeral-storage": quantity(s.EphemeralRequestBytes)},
		"limits":   {"memory": quantity(s.MemoryLimitBytes), "ephemeral-storage": quantity(s.EphemeralLimitBytes)},
	}
}

type Flags struct {
	DeadlineKilled    bool `json:"deadline_killed"`
	LowSamples        bool `json:"low_samples"`
	SampledLowerBound bool `json:"sampled_lower_bound"`
	Saturated         bool `json:"saturated"`
	Restarts          bool `json:"restarts"`
	OOM               bool `json:"oom"`
	LimitReached      bool `json:"limit_reached"`
	RequestsChanged   bool `json:"requests_changed"`
	CounterReset      bool `json:"counter_reset"`
	LimitsUnknown     bool `json:"limits_unknown"`
	IdentityUnknown   bool `json:"identity_unknown"`
}
type Step struct {
	MemoryStepAttempts         int                     `json:"memory_step_peak_attempts"`
	MemorySamplerAttempts      int                     `json:"memory_sampler_attempts"`
	MemoryInsufficientAttempts int                     `json:"memory_insufficient_attempts"`
	Stage                      string                  `json:"burn"`
	Template                   string                  `json:"template"`
	OwnerRepository            string                  `json:"owner_repository"`
	Runs                       int                     `json:"runs"`
	RunIDs                     []string                `json:"run_ids"`
	Attempts                   int                     `json:"attempts"`
	Samples                    int64                   `json:"samples"`
	Since                      time.Time               `json:"since,omitzero"`
	Until                      time.Time               `json:"until,omitzero"`
	Declared                   *Declaration            `json:"declared"`
	MemoryPeakBytes            model.ResourceQuantiles `json:"memory_peak_bytes"`
	AverageCPUNanoCores        model.ResourceQuantiles `json:"average_cpu_nanocores"`
	PeakCPUNanoCores           model.ResourceQuantiles `json:"peak_cpu_nanocores"`
	EphemeralBytes             model.ResourceQuantiles `json:"ephemeral_storage_bytes"`
	DurationNS                 model.ResourceQuantiles `json:"duration_ns"`
	IOReadBytes                model.ResourceQuantiles `json:"io_read_bytes"`
	IOWriteBytes               model.ResourceQuantiles `json:"io_write_bytes"`
	IOPressureSomeNS           model.ResourceQuantiles `json:"io_pressure_some_ns"`
	IOPressureFullNS           model.ResourceQuantiles `json:"io_pressure_full_ns"`
	Flags                      Flags                   `json:"flags"`
	Suggested                  *Suggested              `json:"suggested"`
	NoSuggestion               []string                `json:"no_suggestion"`
}
type Report struct {
	Format             string `json:"-"`
	Repository         string `json:"repository"`
	Ref                string `json:"ref"`
	Run                string `json:"run,omitempty"`
	RequestedRuns      int    `json:"requested_runs"`
	SelectedRuns       int    `json:"selected_runs"`
	NewestSHA          string `json:"newest_sha"`
	IncludeFailed      bool   `json:"include_failed"`
	IncludePromotions  bool   `json:"include_promotions"`
	Truncated          bool   `json:"truncated"`
	DeclarationsStatus string `json:"declarations_status"`
	Retention          string `json:"retention"`
	Semantics          string `json:"semantics"`
	Steps              []Step `json:"steps"`
}

func (r Report) MCPToolText() string {
	if r.Format == "yaml" {
		return YAML(r)
	}
	raw, _ := json.Marshal(r)
	return string(raw)
}

const Retention = "External samples retain at most 90 days / 100,000 container incarnations globally, asynchronously. There is no per-repo or release reservation: busy repositories can displace rare release history. Missing history is unknown. A step-reported memory peak is used only with exact retained attempt identity, and only for advice."
const Semantics = "Read-only. Quantiles are across container attempts, not time samples; each metric reports its own count. Memory prefers an identity-bound step-reported cgroup peak (self-reported, advisory), retaining any higher external peak with at least four polls; otherwise sampler memory requires four polls per attempt. Any missing memory coverage, unknown limits or deadline kill suppresses suggestions. CPU average uses captured CPU time over its measured interval. Ephemeral and IO pressure are pod-scoped, once per main attempt; PVC bytes are excluded. External peaks are sampled lower bounds. Git is the source of truth; suggestions never change execution."

var fragmentName = regexp.MustCompile(`^frag-[0-9a-f]{8}-(.+)$`)
var retrySuffix = regexp.MustCompile(`\([0-9]+\)$`)
var podHash = regexp.MustCompile(`-[0-9]+$`)

func NormalizeTemplate(name string) string {
	name = retrySuffix.ReplaceAllString(name, "")
	if m := fragmentName.FindStringSubmatch(name); len(m) == 2 {
		return m[1]
	}
	return name
}

// Legacy sampler v2 rows did not carry TemplateName. the pinned pod naming
// convention contains the template between the exact workflow prefix and hash.
// A bare invocation name is not substituted when that binding is unavailable.
func TemplateOf(v model.StepResourceUsage) string {
	if v.TemplateName != "" {
		return v.TemplateName
	}
	prefix := v.WorkflowName + "-"
	if strings.HasPrefix(v.PodName, prefix) {
		name := strings.TrimPrefix(v.PodName, prefix)
		if podHash.MatchString(name) {
			return podHash.ReplaceAllString(name, "")
		}
	}
	return ""
}
func key(stage, template string) string {
	return NormalizeTemplate(stage) + "\x00" + NormalizeTemplate(template)
}
func Build(values []Observation, declarations []Declaration) []Step {
	groups := map[string][]Observation{}
	for _, o := range values {
		v := o.Usage
		if (v.Version != 1 && v.Version != 2) || v.Source != "kubelet-cadvisor" || !v.IsWorkflowMain() {
			continue
		}
		if o.Template == "" {
			o.Template = TemplateOf(v)
		}
		if o.Owner == "" {
			o.Owner = v.FragmentRepository
		}
		groups[key(v.Stage, o.Template)] = append(groups[key(v.Stage, o.Template)], o)
	}
	declared := map[string]*Declaration{}
	for i := range declarations {
		d := &declarations[i]
		k := key(d.Stage, d.Template)
		declared[k] = d
		if _, ok := groups[k]; !ok {
			groups[k] = nil
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Step, 0, len(keys))
	for _, k := range keys {
		stage, template, _ := strings.Cut(k, "\x00")
		s := Step{Stage: stage, Template: template, Declared: declared[k], RunIDs: []string{}, NoSuggestion: []string{}}
		if s.Declared != nil {
			s.OwnerRepository = s.Declared.OwnerRepository
		}
		populations := make([][]int64, 9)
		runs := map[string]bool{}
		seen := map[string]bool{}
		metricRuns := make([]map[string]bool, 5)
		for i := range metricRuns {
			metricRuns[i] = map[string]bool{}
		}
		for _, o := range groups[k] {
			v := o.Usage
			s.Flags.IdentityUnknown = s.Flags.IdentityUnknown || o.Ambiguous
			id := v.RunID + "\x00" + v.PodUID + "\x00" + v.ContainerID
			if seen[id] {
				continue
			}
			seen[id] = true
			s.Attempts++
			s.Samples = satAdd(s.Samples, max(v.Samples, 0))
			runs[v.RunID] = true
			if s.Since.IsZero() || v.FirstObservedAt.Before(s.Since) {
				s.Since = v.FirstObservedAt
			}
			if v.LastObservedAt.After(s.Until) {
				s.Until = v.LastObservedAt
			}
			if s.OwnerRepository == "" {
				s.OwnerRepository = o.Owner
			}
			if o.Owner != "" && s.OwnerRepository != o.Owner {
				s.Flags.IdentityUnknown = true
			}
			if o.Owner == "" && strings.HasPrefix(o.Template, "frag-") {
				s.Flags.IdentityUnknown = true
			}
			s.Flags.SampledLowerBound = s.Flags.SampledLowerBound || v.SampledLowerBound
			s.Flags.Saturated = s.Flags.Saturated || v.Saturated
			s.Flags.Restarts = s.Flags.Restarts || v.RestartCount > 0
			s.Flags.OOM = s.Flags.OOM || v.Reason == "OOMKilled"
			s.Flags.DeadlineKilled = s.Flags.DeadlineKilled || o.DeadlineKilled || v.Reason == "DeadlineExceeded"
			s.Flags.RequestsChanged = s.Flags.RequestsChanged || v.RequestsChanged
			s.Flags.CounterReset = s.Flags.CounterReset || v.CounterReset
			if o.MemoryLimitBytes == nil {
				o.MemoryLimitBytes = v.MemoryLimitBytes
			}
			if o.CPULimitNanoCores == nil {
				o.CPULimitNanoCores = v.CPULimitNanoCores
			}
			if o.EphemeralLimitBytes == nil {
				o.EphemeralLimitBytes = v.EphemeralLimitBytes
			}
			s.Flags.LimitsUnknown = s.Flags.LimitsUnknown || o.MemoryLimitBytes == nil || o.CPULimitNanoCores == nil || o.EphemeralLimitBytes == nil
			var memory *int64
			if o.StepMemoryPeakBytes != nil && *o.StepMemoryPeakBytes >= 0 {
				memory = o.StepMemoryPeakBytes
				s.MemoryStepAttempts++
				if v.Samples >= MinimumMemoryPolls && v.MemoryPeakBytes != nil && *v.MemoryPeakBytes > *memory {
					memory = v.MemoryPeakBytes
				}
			} else if v.Samples >= MinimumMemoryPolls && v.MemoryPeakBytes != nil && *v.MemoryPeakBytes >= 0 {
				memory = v.MemoryPeakBytes
				s.MemorySamplerAttempts++
			} else {
				s.MemoryInsufficientAttempts++
				s.Flags.LowSamples = true
			}
			for _, p := range [][2]*int64{{memory, o.MemoryLimitBytes}, {v.MemoryPeakBytes, o.MemoryLimitBytes}, {v.CPUPeakNanoCores, o.CPULimitNanoCores}, {v.EphemeralStoragePeakBytes, o.EphemeralLimitBytes}} {
				if p[0] != nil && p[1] != nil && *p[1] > 0 && *p[0] >= *p[1] {
					s.Flags.LimitReached = true
				}
			}
			var duration, average *int64
			if !v.ContainerStartedAt.IsZero() && !v.ContainerFinishedAt.IsZero() && v.ContainerFinishedAt.After(v.ContainerStartedAt) {
				n := int64(v.ContainerFinishedAt.Sub(v.ContainerStartedAt))
				duration = &n
			}
			if v.CPUTimeNS != nil && *v.CPUTimeNS >= 0 && !v.CounterReset && !v.ContainerStartedAt.IsZero() {
				end := v.CPUTimeAt
				if !v.ContainerFinishedAt.IsZero() && end.After(v.ContainerFinishedAt) {
					end = v.ContainerFinishedAt
				}
				if end.After(v.ContainerStartedAt) {
					n, saturated := model.ResourceRate(*v.CPUTimeNS, end.Sub(v.ContainerStartedAt))
					if !saturated {
						average = &n
					} else {
						s.Flags.Saturated = true
					}
				}
			}
			metrics := []*int64{memory, average, v.CPUPeakNanoCores, v.EphemeralStoragePeakBytes, duration, v.IOReadBytes, v.IOWriteBytes, v.IOPressureSomeNS, v.IOPressureFullNS}
			for i, n := range metrics {
				if n != nil && *n >= 0 {
					populations[i] = append(populations[i], *n)
					if i < 5 && v.Terminal && !v.Saturated && !v.CounterReset {
						metricRuns[i][v.RunID] = true
					}
				}
			}
		}
		for run := range runs {
			s.RunIDs = append(s.RunIDs, run)
		}
		slices.Sort(s.RunIDs)
		s.Runs = len(runs)
		targets := []*model.ResourceQuantiles{&s.MemoryPeakBytes, &s.AverageCPUNanoCores, &s.PeakCPUNanoCores, &s.EphemeralBytes, &s.DurationNS, &s.IOReadBytes, &s.IOWriteBytes, &s.IOPressureSomeNS, &s.IOPressureFullNS}
		for i, p := range populations {
			*targets[i] = model.Quantiles(p)
		}
		s.Flags.IdentityUnknown = s.Flags.IdentityUnknown || s.Template == "" || s.OwnerRepository == ""
		insufficientRuns := false
		for _, i := range []int{0, 1, 3, 4} {
			if len(metricRuns[i]) < MinimumSamples {
				s.Flags.LowSamples = true
				insufficientRuns = true
			}
		}
		if insufficientRuns {
			s.NoSuggestion = append(s.NoSuggestion, fmt.Sprintf("need %d distinct terminal runs with memory, average CPU, ephemeral and duration measurements", MinimumSamples))
		}
		if s.MemoryInsufficientAttempts > 0 {
			s.NoSuggestion = append(s.NoSuggestion, fmt.Sprintf("%d attempts lack a bound step memory peak or at least %d memory polls", s.MemoryInsufficientAttempts, MinimumMemoryPolls))
		}
		if s.Flags.LimitsUnknown {
			s.NoSuggestion = append(s.NoSuggestion, "historical CPU, memory or ephemeral limits are unknown; censoring cannot be ruled out")
		}
		if s.Flags.DeadlineKilled {
			s.NoSuggestion = append(s.NoSuggestion, "deadline-killed attempt duration is censored; measure with a reviewed higher deadline first")
		}
		if s.Flags.LimitReached || s.Flags.OOM {
			s.NoSuggestion = append(s.NoSuggestion, "a peak was censored by a limit or OOM; measure with a reviewed higher limit first")
		}
		if s.Flags.Saturated || s.Flags.CounterReset || s.Flags.RequestsChanged || s.Flags.Restarts {
			s.NoSuggestion = append(s.NoSuggestion, "saturated/reset/changed-request/restarted observations need inspection")
		}
		if s.Flags.IdentityUnknown {
			s.NoSuggestion = append(s.NoSuggestion, "template or owning repository is unknown or conflicting")
		}
		if len(s.NoSuggestion) == 0 {
			cpu := roundMargin(*s.AverageCPUNanoCores.P95, RequestMarginPercent, 10_000_000, 10_000_000) / 1_000_000
			s.Suggested = &Suggested{CPUMilli: cpu, MemoryRequestBytes: roundMargin(*s.MemoryPeakBytes.P95, RequestMarginPercent, 1<<20, 64<<20), MemoryLimitBytes: roundMargin(*s.MemoryPeakBytes.Max, LimitMarginPercent, 1<<20, 64<<20), EphemeralRequestBytes: roundMargin(*s.EphemeralBytes.Max, RequestMarginPercent, 1<<20, 64<<20), EphemeralLimitBytes: roundMargin(*s.EphemeralBytes.Max, LimitMarginPercent, 1<<20, 64<<20), ActiveDeadlineSeconds: satAdd(roundMargin(*s.DurationNS.Max, DeadlineMarginPercent, int64(time.Second), int64(time.Second))/int64(time.Second), DeadlineStartupSeconds)}
		}
		out = append(out, s)
	}
	return out
}

// WithDiagnostic uses only the server's numeric pod-spec/timing observation,
// bound to the exact incarnation. The termination message is never consulted.
func WithDiagnostic(o Observation, d *model.StepDiagnostic) Observation {
	if d == nil || !d.Available || d.Validate() != nil || d.Container == nil {
		return o
	}
	c := d.Container
	v := &o.Usage
	if c.RunID != v.RunID || c.WorkflowUID != v.WorkflowUID || c.PodUID != v.PodUID || c.ContainerID != v.ContainerID || c.ContainerName != "main" || c.RestartCount != v.RestartCount {
		return o
	}
	o.DeadlineKilled = o.DeadlineKilled || c.PodReason == "DeadlineExceeded"
	if c.CPULimitMilli != nil && *c.CPULimitMilli <= 1_000_000 {
		n := *c.CPULimitMilli * 1_000_000
		o.CPULimitNanoCores = &n
	}
	o.MemoryLimitBytes = c.MemoryLimitBytes
	if v.ContainerStartedAt.IsZero() {
		v.ContainerStartedAt = c.StartedAt
	}
	if v.ContainerFinishedAt.IsZero() {
		v.ContainerFinishedAt = c.FinishedAt
	}
	return o
}

// WithStepResult permits the step's own cgroup peak for reviewed suggestions,
// never admission. A final display row must not supply a peak to an older retry.
func WithStepResult(o Observation, d *model.StepDiagnostic, memory *int64, status model.StepStatus) Observation {
	o = WithDiagnostic(o, d)
	if d == nil || !d.Available || d.Validate() != nil || d.Container == nil {
		return o
	}
	c, v := d.Container, o.Usage
	if c.RunID != v.RunID || c.WorkflowUID != v.WorkflowUID || c.PodUID != v.PodUID || c.ContainerID != v.ContainerID || c.ContainerName != "main" || c.RestartCount != v.RestartCount {
		return o
	}
	if memory != nil && *memory >= 0 {
		o.StepMemoryPeakBytes = memory
	}
	o.DeadlineKilled = o.DeadlineKilled || status == model.StepTimedOut
	return o
}
func satAdd(a, b int64) int64 {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
func roundMargin(n, percent, unit, floor int64) int64 {
	if n > math.MaxInt64/(100+percent) {
		return math.MaxInt64 / unit * unit
	}
	n = (n*(100+percent) + 99) / 100
	if n > math.MaxInt64-unit {
		return math.MaxInt64 / unit * unit
	}
	return max(floor, (n+unit-1)/unit*unit)
}

// YAML preserves the report and adds ordinary pod blocks for copy/paste.
// It has no execution authority and introduces no pipeline syntax.
func YAML(r Report) string {
	raw, _ := json.Marshal(r)
	var document map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	_ = decoder.Decode(&document)
	steps, _ := document["steps"].([]any)
	for i, s := range r.Steps {
		if s.Suggested != nil && i < len(steps) {
			step, _ := steps[i].(map[string]any)
			step["paste_ready"] = map[string]any{"resources": s.Suggested.Resources(), "activeDeadlineSeconds": s.Suggested.ActiveDeadlineSeconds}
		}
	}
	output, _ := yaml.Marshal(document)
	return string(output)
}
