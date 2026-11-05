package measured

import "encoding/json"

// SizingAcceptance describes the evidence, not authority to change execution.
// Runs must contain all required measurements together. Counting each metric
// separately can accidentally combine disjoint, incomplete histories.
type SizingAcceptance struct {
	Status                 string   `json:"status"`
	Sufficient             bool     `json:"sufficient"`
	RequiredRuns           int      `json:"required_runs"`
	ComparableCompleteRuns int      `json:"comparable_complete_runs"`
	Cohorts                int      `json:"cohorts"`
	CensoredAttempts       int      `json:"censored_attempts"`
	Reasons                []string `json:"reasons"`
}

type sizingHistory struct {
	cohorts  map[string]map[string]bool
	censored int
}

func (h *sizingHistory) observe(o Observation, complete, censored bool) {
	if censored {
		h.censored++
	}
	if o.CPULimitNanoCores == nil || o.MemoryLimitBytes == nil || o.EphemeralLimitBytes == nil {
		return
	}
	if *o.CPULimitNanoCores < 0 || *o.MemoryLimitBytes < 0 || *o.EphemeralLimitBytes < 0 {
		return
	}
	v := o.Usage
	// Compare the observed conditions, never today's declarations substituted
	// for unknown history. Nil requests remain distinct from explicit zero.
	encoded, _ := json.Marshal(struct {
		CPU, Memory, Storage, CPULimit, MemoryLimit, StorageLimit *int64
		Size                                                      string
	}{v.CPURequestNanoCores, v.MemoryRequestBytes, v.EphemeralRequestBytes,
		o.CPULimitNanoCores, o.MemoryLimitBytes, o.EphemeralLimitBytes, string(v.WorkflowSize)})
	if h.cohorts == nil {
		h.cohorts = map[string]map[string]bool{}
	}
	key := string(encoded)
	if h.cohorts[key] == nil {
		h.cohorts[key] = map[string]bool{}
	}
	if complete && !censored && !o.Ambiguous && !v.Saturated && !v.CounterReset && !v.RequestsChanged && !v.SizeClassChanged && v.RestartCount == 0 && v.RunID != "" {
		h.cohorts[key][v.RunID] = true
	}
}

func (h sizingHistory) count() int {
	count := 0
	for _, runs := range h.cohorts {
		count = max(count, len(runs))
	}
	return count
}

func (h sizingHistory) acceptance(reasons []string) SizingAcceptance {
	a := SizingAcceptance{Status: "sufficient", Sufficient: len(reasons) == 0,
		RequiredRuns: MinimumSamples, ComparableCompleteRuns: h.count(),
		Cohorts: len(h.cohorts), CensoredAttempts: h.censored, Reasons: append([]string{}, reasons...)}
	switch {
	case h.censored > 0:
		a.Status = "censored"
	case len(h.cohorts) > 1:
		a.Status = "non_comparable"
	case !a.Sufficient:
		a.Status = "insufficient"
	}
	return a
}

// RefuseSizing keeps wire acceptance consistent when a caller discovers a
// report-wide gap, such as truncated history, after grouping the observations.
func (s *Step) RefuseSizing(reason string) {
	s.Suggested = nil
	s.AdviceBasis = ""
	s.NoSuggestion = append(s.NoSuggestion, reason)
	s.SizingAcceptance.Sufficient = false
	if s.SizingAcceptance.Status == "sufficient" {
		s.SizingAcceptance.Status = "insufficient"
	}
	s.SizingAcceptance.Reasons = append(s.SizingAcceptance.Reasons, reason)
}
