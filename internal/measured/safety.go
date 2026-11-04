package measured

import (
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const SafetyScope = "Safety checks consult up to 100 latest matching terminal runs before include_failed filtering. Missing executed-attempt evidence or a window that does not cover the displayed terminal runs refuses advice. This bounded retained history is not proof of cold-run sufficiency."

func applySafety(s *Step, h *sizingHistory, safety []Observation, dispositions []model.PodResourceDisposition, displayed []Observation) {
	seen := map[string]bool{}
	usagePods := map[string]bool{}
	reasons := map[string]bool{}
	for _, r := range s.NoSuggestion {
		reasons[r] = true
	}
	for _, o := range displayed {
		usagePods[o.Usage.RunID+"\x00"+o.Usage.PodUID] = true
	}
	for _, o := range safety {
		usagePods[o.Usage.RunID+"\x00"+o.Usage.PodUID] = true
	}
	for _, o := range displayed {
		if o.DeadlineKilled || o.OOMKilled || o.Usage.Reason == "OOMKilled" || observationAtLimit(withUsageLimits(o)) {
			seen[o.Usage.RunID+"\x00"+o.Usage.PodUID] = true
		}
	}
	refuse := func(reason string) {
		if !reasons[reason] {
			s.NoSuggestion = append(s.NoSuggestion, reason)
			reasons[reason] = true
		}
	}
	for _, o := range safety {
		o = withUsageLimits(o)
		if (o.Owner != "" && s.OwnerRepository != "" && o.Owner != s.OwnerRepository) || (o.Owner == "" && strings.HasPrefix(o.Template, "frag-")) {
			s.Flags.IdentityUnknown = true
			refuse("safety history owning repository is unknown or conflicts with displayed provenance")
		}
		v := o.Usage
		id := v.RunID + "\x00" + v.PodUID
		atLimit := observationAtLimit(o)
		censored := o.DeadlineKilled || o.OOMKilled || v.Reason == "OOMKilled" || atLimit
		if censored {
			if !seen[id] {
				h.censored++
				seen[id] = true
			}
			s.Flags.DeadlineKilled = s.Flags.DeadlineKilled || o.DeadlineKilled
			s.Flags.OOM = s.Flags.OOM || o.OOMKilled || v.Reason == "OOMKilled"
			s.Flags.LimitReached = s.Flags.LimitReached || atLimit
			refuse("retained safety history contains a censored attempt, independent of displayed failed metrics")
		} else if (o.RunTerminal && !v.Terminal) || (v.Terminal && (!coveredMemory(o) || !coveredCPU(v) || !coveredStorage(v) || (v.ExitCode != nil && *v.ExitCode != 0) || (v.Reason != "" && v.Reason != "Completed"))) {
			refuse("retained safety history has failed or incomplete workload/metric coverage")
		}
		if v.Saturated || v.CounterReset || v.RequestsChanged || v.SizeClassChanged || v.RestartCount > 0 || o.Ambiguous {
			refuse("retained safety history contains saturated/reset/changed/restarted or ambiguous observations")
		}
	}
	for _, d := range dispositions {
		id := d.RunID + "\x00" + d.PodUID
		if s.OwnerRepository == "" {
			s.OwnerRepository = d.Owner
		}
		if (d.Owner != "" && s.OwnerRepository != "" && d.Owner != s.OwnerRepository) || (d.Owner == "" && strings.HasPrefix(d.Template, "frag-")) {
			s.Flags.IdentityUnknown = true
			refuse("Pod disposition owning repository is unknown or conflicts with displayed provenance")
		}
		if d.Reason != "Completed" {
			refuse("server-observed Pod disposition is failed; no completed-workload resource bound is established")
			if d.Reason == "Evicted" || d.Reason == "DeadlineExceeded" {
				if !seen[id] {
					h.censored++
					seen[id] = true
				}
				s.Flags.DeadlineKilled = s.Flags.DeadlineKilled || d.Reason == "DeadlineExceeded"
			}
			continue
		}
		found := usagePods[id]
		if !found {
			refuse("executed Pod has no retained main usage row; missing measurements remain unknown")
		}
	}
}

func withUsageLimits(o Observation) Observation {
	if o.MemoryLimitBytes == nil {
		o.MemoryLimitBytes = o.Usage.MemoryLimitBytes
	}
	if o.CPULimitNanoCores == nil {
		o.CPULimitNanoCores = o.Usage.CPULimitNanoCores
	}
	if o.EphemeralLimitBytes == nil {
		o.EphemeralLimitBytes = o.Usage.EphemeralLimitBytes
	}
	return o
}

func observationAtLimit(o Observation) bool {
	v := o.Usage
	for _, p := range [][2]*int64{{v.MemoryPeakBytes, o.MemoryLimitBytes}, {o.StepMemoryPeakBytes, o.MemoryLimitBytes}, {v.CPUPeakNanoCores, o.CPULimitNanoCores}, {v.EphemeralStoragePeakBytes, v.PodEphemeralLimitBytes}} {
		if p[0] != nil && p[1] != nil && *p[1] > 0 && *p[0] >= *p[1] {
			return true
		}
	}
	return false
}
