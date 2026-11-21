package argojob

import (
	core "k8s.io/api/core/v1"
	"time"
)

const terminalEvidenceGrace = time.Minute

// A terminal fact publishes immediately. Independent Pod watches can improve
// evidence during this window; one delayed notification bounds missing evidence.
func (e *Executor) evidenceSettled(t *tracking, o Observation, pods []*core.Pod) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return t.completed && (len(o.Nodes) == 0 || o.Complete && len(t.pending) == 0 && podCoverage(o.workflow, pods) || e.expired(t))
}
func (e *Executor) expired(t *tracking) bool {
	return t.completed && !t.evidenceUntil.IsZero() && !e.config.Now().Before(t.evidenceUntil)
}
