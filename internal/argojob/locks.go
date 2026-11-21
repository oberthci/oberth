package argojob

import (
	"slices"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// LockRole says how one Workflow stands toward one native lock.
type LockRole string

const (
	// LockHolder holds units of the lock by its own status.
	LockHolder LockRole = "holder"
	// LockWaiter waits for the lock.
	LockWaiter LockRole = "waiter"
	// LockStale has ended, yet the workflow controller still counts a unit
	// of the lock for it: a waiter's status names it among the holders.
	LockStale LockRole = "stale"
)

// LockFact is one native synchronization fact; Workflow points into the rows read.
type LockFact struct {
	Lock     string
	Role     LockRole
	Workflow *wf.Workflow
}

// LockFacts reads current holders and waiters from Workflow statuses. An ended
// Workflow is never a holder by its own status: the controller clears that
// status when the Workflow ends. It is stale when a live waiter's status, which
// the controller writes from its own count, still names it as a holder.
func LockFacts(rows []wf.Workflow) []LockFact {
	c := lockCollector{byKey: make(map[string]*wf.Workflow, len(rows)), out: []LockFact{}}
	for i := range rows {
		c.byKey[rows[i].Namespace+"/"+rows[i].Name] = &rows[i]
	}
	for i := range rows {
		c.workflow(&rows[i])
	}
	return c.out
}

type lockCollector struct {
	byKey map[string]*wf.Workflow
	out   []LockFact
}

func (c *lockCollector) add(f LockFact) {
	if !slices.Contains(c.out, f) {
		c.out = append(c.out, f)
	}
}

func (c *lockCollector) workflow(w *wf.Workflow) {
	if w.Status.Synchronization == nil || terminalWorkflow(w) {
		return
	}
	held, waited := lockEntries(w.Status.Synchronization)
	for _, e := range held {
		if slices.ContainsFunc(e.holders, func(h string) bool { return h != "" }) {
			c.add(LockFact{Lock: e.lock, Role: LockHolder, Workflow: w})
		}
	}
	for _, e := range waited {
		c.add(LockFact{Lock: e.lock, Role: LockWaiter, Workflow: w})
		for _, holder := range e.holders {
			if other := c.byKey[holderWorkflow(holder)]; other != nil && terminalWorkflow(other) {
				c.add(LockFact{Lock: e.lock, Role: LockStale, Workflow: other})
			}
		}
	}
}

type lockEntry struct {
	lock    string
	holders []string
}

func lockEntries(s *wf.SynchronizationStatus) (held, waited []lockEntry) {
	if s.Semaphore != nil {
		for _, h := range s.Semaphore.Holding {
			held = append(held, lockEntry{h.Semaphore, h.Holders})
		}
		for _, h := range s.Semaphore.Waiting {
			waited = append(waited, lockEntry{h.Semaphore, h.Holders})
		}
	}
	if s.Mutex != nil {
		for _, h := range s.Mutex.Holding {
			held = append(held, lockEntry{h.Mutex, []string{h.Holder}})
		}
		for _, h := range s.Mutex.Waiting {
			waited = append(waited, lockEntry{h.Mutex, []string{h.Holder}})
		}
	}
	return held, waited
}

// holderWorkflow maps a controller holder key, namespace/workflow or
// namespace/workflow/node, to its namespace/workflow.
func holderWorkflow(key string) string {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
