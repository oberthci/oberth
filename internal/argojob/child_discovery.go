package argojob

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"k8s.io/apimachinery/pkg/types"
)

type childNotice struct {
	uid       types.UID
	permanent bool
}

// Notices live only while the same native UID remains in the watch cache.
func (e *Executor) changed(name string) {
	e.mu.Lock()
	notice, seen := e.childNotices[name]
	e.mu.Unlock()
	if seen {
		w, ok := e.config.Cache.Workflow(name)
		e.mu.Lock()
		if e.childNotices[name] == notice && (!ok || w.UID != notice.uid) {
			delete(e.childNotices, name)
		}
		e.mu.Unlock()
	}
	e.queue.Add(name)
}

func (e *Executor) discoverChild(ctx context.Context, name string) {
	if e.config.Children == nil || e.config.ChildRuns == nil || e.config.Keeper == nil || e.config.Cache == nil {
		e.queue.Forget(name)
		return
	}
	w, reported := e.childCandidate(name)
	if w == nil {
		e.queue.Forget(name)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, e.config.ObservationTimeout)
	defer cancel()
	err := e.recordChild(ctx, w)
	permanent := childRefusal(err)
	e.mu.Lock()
	if err == nil {
		delete(e.childNotices, name)
	} else {
		e.childNotices[name] = childNotice{uid: w.UID, permanent: permanent}
	}
	e.mu.Unlock()
	if err != nil && !reported {
		e.config.Logger.ErrorContext(ctx, childNotRecorded, "workflow", name, "reason", publicerr.Classify(err).Reason(), "remedy", childRemedy, "cause", err)
	}
	if err != nil && !permanent {
		e.queue.AddRateLimited(name)
		return
	}
	e.queue.Forget(name)
}

func (e *Executor) childCandidate(name string) (*wf.Workflow, bool) {
	w, ok := e.config.Cache.Workflow(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !ok {
		delete(e.childNotices, name)
		return nil, false
	}
	notice, seen := e.childNotices[name]
	reported := seen && notice.uid == w.UID
	if w.Labels[nativebinding.NativeCronLabel] == "" || reported && notice.permanent {
		return nil, reported
	}
	return w, reported
}

func childRefusal(err error) bool {
	var refusal *nativebinding.Refusal
	return errors.As(err, &refusal) || publicerr.Permanent(err)
}
