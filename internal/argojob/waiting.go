package argojob

import (
	"fmt"
	"slices"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/publicerr"
)

// throttledMessage is the pinned controller's message on a Workflow it keeps
// Pending because the installation's run cap, Argo parallelism, is reached.
const throttledMessage = "Workflow processing has been postponed because too many workflows are already running"

// waitingLocks maps each logical path to the lock a node there waits for,
// whatever the node's type: a lock on a retried or nested template is held by
// a structural node before any Pod exists. A node that waits is Pending.
func waitingLocks(w *wf.Workflow, paths nativePaths) map[string]string {
	waits := map[string]string{}
	for _, n := range w.Status.Nodes {
		if n.Phase != wf.NodePending || n.SynchronizationStatus == nil || n.SynchronizationStatus.Waiting == "" {
			continue
		}
		path := paths[n.ID].name
		if old, ok := waits[path]; !ok || n.SynchronizationStatus.Waiting < old {
			waits[path] = n.SynchronizationStatus.Waiting
		}
	}
	return waits
}

// lockFor is the lock that path, or a node enclosing it, waits for.
func lockFor(waits map[string]string, path string) string {
	found := []string{}
	for base, lock := range waits {
		if underPath(path, base) {
			found = append(found, lock)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return slices.Min(found)
}

// waitingFor names what a step without an observed node waits for when Argo
// tells: a lock its own or an enclosing node waits for, a lock the pending
// Workflow waits for, or a run slot; otherwise it is waiting to start.
func waitingFor(w *wf.Workflow, waits map[string]string, path string) string {
	if lock := lockFor(waits, path); lock != "" {
		return lockReason(lock)
	}
	if w.Status.Phase != "" && w.Status.Phase != wf.WorkflowPending {
		return reasonWaiting
	}
	if s := w.Status.Synchronization; s != nil {
		_, waited := lockEntries(s)
		if len(waited) > 0 {
			return lockReason(waited[0].lock)
		}
	}
	if w.Status.Message == throttledMessage {
		return reasonRunSlot
	}
	return reasonWaiting
}

// waitingDetail points a step waiting for a run slot to the native message
// and one waiting for a lock to lock status.
func waitingDetail(w *wf.Workflow, reason string) string {
	if reason == reasonRunSlot {
		return w.Status.Message
	}
	return detailLockStatus
}

// lockReason names a lock by the controller's key: a semaphore,
// namespace/ConfigMap/name/key, by its key among the sync limits, a mutex,
// namespace/Mutex/name, by its name. A name that is not a plain key or does
// not fit the 40 characters of a reason leaves the general phrase.
func lockReason(lock string) string {
	parts := strings.Split(lock, "/")
	reason := reasonLock
	switch {
	case len(parts) == 4 && parts[1] == "ConfigMap" && plainName(parts[3]):
		reason = fmt.Sprintf(reasonSyncLimit, parts[3])
	case len(parts) == 3 && parts[1] == "Mutex" && plainName(parts[2]):
		reason = fmt.Sprintf(reasonMutex, parts[2])
	}
	if publicerr.CheckMessage(reason, 40) != nil {
		return reasonLock
	}
	return reason
}

func plainName(name string) bool {
	return name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == ""
}

// queuedReason is a queued run's reason: the first step, in plan order, that
// says what it waits for, else waiting to start.
func queuedReason(rows []projected) string {
	for _, r := range rows {
		if !r.step.Started() && r.step.DisplayReason != "" && r.step.DisplayReason != reasonWaiting {
			return r.step.DisplayReason
		}
	}
	return reasonWaiting
}
