package argojob

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Collection cannot turn a timeout, an interrupted copy or a cache miss into
// permission to delete a native log source. TTL also collects its dependent Pods.
func (e *Executor) requireArchives(ctx context.Context, b nativebinding.RunBinding, w *wf.Workflow) error {
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return err
	}
	if w.Status.OffloadNodeStatusVersion != "" {
		return errors.New("native nodes are not available")
	}
	entries, err := e.collectionLogEntries(ctx, b, w)
	if err != nil {
		return err
	}
	return e.archivedEntries(ctx, b, entries)
}

func (e *Executor) archivedEntries(ctx context.Context, b nativebinding.RunBinding, entries []runlog.ContainerLog) error {
	if len(entries) == 0 {
		return nil
	}
	if e.config.Logs == nil {
		return errArchivePending
	}
	ready, err := e.config.Logs.Archived(ctx, b.RunID, entries)
	if err != nil {
		return err
	}
	if !ready {
		return errArchivePending
	}
	return nil
}

func archivePodFinished(p *core.Pod) bool {
	if p.Status.Phase != core.PodFailed {
		return capturedStatuses(p)
	}
	statuses := slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses)
	if len(statuses) == 0 {
		return false
	}
	return !slices.ContainsFunc(statuses, func(c core.ContainerStatus) bool { return c.State.Running != nil })
}

func (e *Executor) collectionLogEntries(ctx context.Context, b nativebinding.RunBinding, w *wf.Workflow) ([]runlog.ContainerLog, error) {
	entries, _, err := e.collectionSources(ctx, b, w)
	return entries, err
}

func (e *Executor) collectionSources(ctx context.Context, b nativebinding.RunBinding, w *wf.Workflow) ([]runlog.ContainerLog, []*core.Pod, error) {
	if e.config.Cache == nil {
		return nil, nil, errors.New("step log sources are unavailable")
	}
	pods := e.config.Cache.Pods(w.Name)
	old, err := e.collectionReceipts(ctx, b.RunID)
	if err != nil {
		return nil, pods, err
	}
	scan := collectionScan{executor: e, binding: b, workflow: w, pods: pods, old: old, paths: indexPaths(w)}
	nodes := slices.Collect(maps.Values(w.Status.Nodes))
	slices.SortFunc(nodes, func(a, b wf.NodeStatus) int { return cmp.Compare(a.ID, b.ID) })
	for _, n := range nodes {
		if n.Type != wf.NodeTypePod {
			continue
		}
		if err := scan.node(ctx, n); err != nil {
			return scan.entries, scan.pods, err
		}
	}
	return scan.entries, scan.pods, nil
}

type collectionScan struct {
	executor *Executor
	binding  nativebinding.RunBinding
	workflow *wf.Workflow
	pods     []*core.Pod
	entries  []runlog.ContainerLog
	reads    int
	old      map[string][]runlog.ContainerLog
	paths    nativePaths
}

func (s *collectionScan) node(ctx context.Context, n wf.NodeStatus) error {
	e, w := s.executor, s.workflow
	p := boundPod(w, n, s.pods)
	if p == nil && capturedNode(n, s.old[n.ID]) {
		s.entries = append(s.entries, s.old[n.ID]...)
		return nil
	}
	if p == nil {
		if s.reads >= collectionBatch {
			return errCollectionPending
		}
		s.reads++
		var err error
		p, err = e.collectionPod(ctx, w, n, s.pods)
		if err != nil {
			return err
		}
	}
	if p == nil {
		missing, err := s.archiveAbsentNode(ctx, n, s.old[n.ID])
		s.entries = append(s.entries, missing...)
		return err
	}
	s.pods = withCollectionPod(s.pods, p)
	if !archivePodFinished(p) {
		return errors.New("step log source is not settled")
	}
	for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		if c.State.Terminated != nil {
			s.entries = append(s.entries, runlog.ContainerLog{NodeID: n.ID, Step: s.paths[n.ID].name, PodUID: string(p.UID), PodName: p.Name, Namespace: p.Namespace, Container: c.Name})
		}
	}
	return nil
}

func withCollectionPod(pods []*core.Pod, p *core.Pod) []*core.Pod {
	at := slices.IndexFunc(pods, func(old *core.Pod) bool { return old.UID == p.UID })
	if at >= 0 {
		pods[at] = p
		return pods
	}
	return append(pods, p)
}

func capturedNode(n wf.NodeStatus, entries []runlog.ContainerLog) bool {
	return n.Fulfilled() && len(entries) > 0 && !slices.ContainsFunc(entries, func(e runlog.ContainerLog) bool { return e.State != runlog.Captured })
}

func (e *Executor) collectionReceipts(ctx context.Context, id string) (map[string][]runlog.ContainerLog, error) {
	byNode := map[string][]runlog.ContainerLog{}
	if e.config.Logs == nil {
		return byNode, nil
	}
	entries, err := e.config.Logs.Containers(ctx, id)
	for _, entry := range entries {
		byNode[entry.NodeID] = append(byNode[entry.NodeID], entry)
	}
	return byNode, err
}

func (e *Executor) collectionPod(ctx context.Context, w *wf.Workflow, n wf.NodeStatus, pods []*core.Pod) (*core.Pod, error) {
	if p := boundPod(w, n, pods); p != nil {
		return p, nil
	}
	if e.config.Pods == nil {
		return nil, errors.New("step log sources are unavailable")
	}
	if err := e.collectionRead(ctx); err != nil {
		return nil, err
	}
	name := argoutil.GeneratePodName(w.Name, n.Name, argoutil.GetTemplateFromNode(n), n.ID, argoutil.GetWorkflowPodNameVersion(w))
	p, err := e.config.Pods.Get(ctx, name, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !podOwned(w, n, p) {
		return nil, ownership(nil)
	}
	return p, nil
}

func (s *collectionScan) archiveAbsentNode(ctx context.Context, n wf.NodeStatus, entries []runlog.ContainerLog) ([]runlog.ContainerLog, error) {
	e, b, w := s.executor, s.binding, s.workflow
	if !n.Fulfilled() || e.config.Logs == nil {
		return nil, errors.New("step log source is not settled")
	}
	gap := runlog.Gap{Reason: runlog.PodMissing, Detail: "The terminal step's Pod is absent from the API."}
	if len(entries) == 0 {
		entries = []runlog.ContainerLog{{NodeID: n.ID, Step: s.paths[n.ID].name, Namespace: w.Namespace, PodName: n.ID, PodUID: "absent-" + n.ID, Container: "main", Main: true}}
		if n.HostNodeName == "" && n.Phase != wf.NodeSucceeded && (n.Outputs == nil || n.Outputs.ExitCode == nil) {
			gap = runlog.Gap{Reason: runlog.NotRun}
		}
	}
	for _, entry := range entries {
		if entry.State == runlog.Captured {
			continue
		}
		if err := e.config.Logs.RecordGap(ctx, b.RunID, entry, gap); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// Finished runs are read-only history. Collection resumes only their log copies;
// it never reprojects steps or opens another native-evidence write sequence.
func (e *Executor) observeArchive(ctx context.Context, t *tracking) error {
	if err := e.collectionRead(ctx); err != nil {
		return err
	}
	w, err := e.Adopt(ctx, t.binding)
	if errors.As(err, new(ErrAbsent)) {
		t.settled = true
		return nil
	}
	if err != nil {
		return err
	}
	if err = packer.DecompressWorkflow(ctx, w); err != nil {
		return err
	}
	if w.Status.OffloadNodeStatusVersion != "" {
		return errors.New("native nodes are not available")
	}
	entries, pods, err := e.collectionSources(ctx, t.binding, w)
	e.capture(ctx, t, w, pods)
	if err != nil {
		return err
	}
	if err = e.archivedEntries(ctx, t.binding, entries); err != nil {
		return err
	}
	if collectionAllowed(w, t.historical) {
		if err = e.releaseCollected(ctx, t.binding, w); err != nil {
			return err
		}
	}
	t.settled = true
	return nil
}
