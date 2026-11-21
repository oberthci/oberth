package argojob

import (
	"context"
	"slices"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
)

// LockRole says how one Workflow stands toward one native lock.
type LockRole string

const (
	// LockHolder holds units of the lock by its own status.
	LockHolder LockRole = "holder"
	// LockWaiter waits for the lock.
	LockWaiter LockRole = "waiter"
	// LockStale has ended, yet the workflow controller still counts a unit
	// of the lock for it: a real waiter's count names it besides every live holder.
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
// status when the Workflow ends. A Workflow waits only while it is Pending on
// a workflow-level lock or one of its nodes waits for that lock; the controller
// keeps a waiting entry after the lock is acquired. A real waiter's entry is
// the controller's count of holders when it last wrote it; when that count names
// every live holder, each ended Workflow it also names is a unit neither held
// nor free: stale. A count older than a live holder proves nothing. Rows are
// decompressed in place so node lock status is read.
func LockFacts(ctx context.Context, rows []wf.Workflow) ([]LockFact, error) {
	c := lockCollector{byKey: make(map[string]*wf.Workflow, len(rows)), live: map[string][]string{}, out: []LockFact{}}
	for i := range rows {
		if err := packer.DecompressWorkflow(ctx, &rows[i]); err != nil {
			return nil, err
		}
		c.byKey[rows[i].Namespace+"/"+rows[i].Name] = &rows[i]
	}
	for i := range rows {
		c.workflow(&rows[i])
	}
	for _, e := range c.counts {
		c.stale(e)
	}
	return c.out, nil
}

type lockCollector struct {
	byKey  map[string]*wf.Workflow
	live   map[string][]string
	counts []lockEntry
	out    []LockFact
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
		for _, h := range e.holders {
			if h != "" {
				c.live[e.lock] = append(c.live[e.lock], h)
				c.add(LockFact{Lock: e.lock, Role: LockHolder, Workflow: w})
			}
		}
	}
	for _, e := range waited {
		if waitsFor(w, e.lock) {
			c.add(LockFact{Lock: e.lock, Role: LockWaiter, Workflow: w})
			c.counts = append(c.counts, e)
		}
	}
}

func (c *lockCollector) stale(e lockEntry) {
	for _, h := range c.live[e.lock] {
		if !slices.Contains(e.holders, h) {
			return
		}
	}
	for _, holder := range e.holders {
		if other := c.byKey[holderWorkflow(holder)]; other != nil && terminalWorkflow(other) {
			c.add(LockFact{Lock: e.lock, Role: LockStale, Workflow: other})
		}
	}
}

// waitsFor reports a Workflow pending on a workflow-level lock or with a node
// whose lock status still waits for lock.
func waitsFor(w *wf.Workflow, lock string) bool {
	if w.Status.Phase == "" || w.Status.Phase == wf.WorkflowPending {
		return true
	}
	for _, n := range w.Status.Nodes {
		if n.SynchronizationStatus != nil && n.SynchronizationStatus.Waiting == lock {
			return true
		}
	}
	return false
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
