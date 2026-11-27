package main

import (
	"context"
	"io"
	"slices"
	"sync"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type serverLogs struct{ owner *serverOwners }

func (l serverLogs) Read(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
	if old, carried, err := l.carriedLogs(ctx, r, q); err != nil {
		return apicontract.LogResult{}, err
	} else if carried {
		out, err := l.Read(ctx, old, q)
		out.ID = r.ID
		return out, err
	}
	out, err := l.read(ctx, r, q)
	usage, usageErr := l.Usage(ctx, r)
	if usageErr == nil {
		out.Usage = &usage
	}
	if err == nil {
		err = usageErr
	}
	return out, err
}

func (l serverLogs) read(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
	request := logRequest(q)
	if q.Containers {
		return l.listContainers(ctx, r.ID)
	}

	steps, err := l.owner.c.store.Steps(ctx, r.ID)
	if err != nil {
		return apicontract.LogResult{}, err
	}
	request = selectPlannedStep(r, steps, request)
	if request.Selector.NodeID == "" && !request.Selector.StepSet {
		request.Selector = focusLogStep(r, steps, request.Selector)
	}
	entries, err := l.owner.c.logs.Containers(ctx, r.ID)
	if err != nil {
		return apicontract.LogResult{}, err
	}
	var containers []apicontract.Container
	request.Selector, containers = defaultLogContainer(steps, request.Selector, entries)
	request.OwnOnly = request.Selector.Container == ""
	body, meta, err := l.owner.c.logs.Read(ctx, r.ID, request)
	if err != nil && q.Container == "" && defaultLogUnavailable(err, r, containers) {
		meta = endedLogMeta(r, steps, request.Selector, meta)
		err = nil
	}
	return apicontract.LogResult{ID: r.ID, Content: string(body), Meta: meta, Containers: containers}, err
}

// selectPlannedStep lists the plan and observed step paths and resolves a step
// selected by the name run get lists to its plan path.
func selectPlannedStep(r stateport.Run, steps []stepmodel.RunStep, request runlog.ReadRequest) runlog.ReadRequest {
	for _, e := range r.Admission.Plan.Entries {
		request.PlannedSteps = append(request.PlannedSteps, e.Path)
	}
	for _, step := range steps {
		if !slices.Contains(request.PlannedSteps, step.Path) {
			request.PlannedSteps = append(request.PlannedSteps, step.Path)
		}
	}
	if request.Selector.StepSet && !slices.Contains(request.PlannedSteps, request.Selector.Step) {
		request.Selector.Step = authoredStepPath(steps, request.Selector.Step)
	}
	return request
}

// authoredStepPath is the plan path of the one step with this name; an unknown
// name and a name several steps share stay as given.
func authoredStepPath(steps []stepmodel.RunStep, step string) string {
	path, found := step, false
	for _, s := range steps {
		if s.Name != step {
			continue
		}
		if found {
			return step
		}
		path, found = s.Path, true
	}
	return path
}
func (l serverLogs) listContainers(ctx context.Context, id string) (apicontract.LogResult, error) {
	entries, err := l.owner.c.logs.Containers(ctx, id)
	out := apicontract.LogResult{ID: id, Containers: []apicontract.Container{}}
	for _, entry := range entries {
		out.Containers = append(out.Containers, apicontract.Container{NodeID: entry.NodeID, PodName: entry.PodName, Container: entry.Container})
	}
	return out, err
}

func logRequest(q apicontract.RunLogsArgs) runlog.ReadRequest {
	request := runlog.ReadRequest{Selector: logcontract.Selector{NodeID: q.NodeID, Container: q.Container}}
	if q.Step != "" {
		request.Selector.Step = q.Step
		request.Selector.StepSet = true
	}
	if q.Pattern != nil {
		request.Filter.Pattern = *q.Pattern
	}
	if q.Context != nil {
		request.Filter.Context = *q.Context
	}
	if q.Offset != nil {
		request.Filter.Offset = *q.Offset
	}
	if q.Limit != nil {
		request.Filter.Limit = *q.Limit
	}
	if q.Tail != nil {
		request.Filter.Tail = *q.Tail
	}
	request.Filter.ByteOffset = q.ByteOffset
	return request
}
func (l serverLogs) Purge(ctx context.Context, r stateport.Run) error {
	_, err := l.owner.c.logs.Purge(ctx, r.ID)
	return err
}

type serverStreams struct{ owner *serverOwners }

func (s serverStreams) CheckLog(ctx context.Context, c apicontract.Caller, id string) error {
	call, err := s.owner.service.PrepareRunGet(ctx, c, apicontract.RunGetArgs{ID: id})
	if err != nil {
		return err
	}
	_, err = call(ctx)
	if err != nil {
		return err
	}
	receipt, err := s.owner.c.store.PurgeReceipt(ctx, id)
	if err == nil {
		if receipt.Completed {
			return publicerr.New(publicerr.Conflict, "Run logs have been purged.", "Rerun the pipeline to capture new logs.", nil)
		}
		return nil
	}
	if missingStore(err) {
		return nil
	}
	return err
}
func (s serverStreams) OpenLog(ctx context.Context, c apicontract.Caller, q apicontract.ContainerArgs) (io.ReadCloser, error) {
	if err := s.CheckLog(ctx, c, q.ID); err != nil {
		return nil, err
	}
	r, err := s.owner.c.store.Run(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	entries, err := s.owner.c.logs.Containers(ctx, q.ID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.NodeID != q.NodeID || entry.Container != q.Container {
			continue
		}
		binding, found, err := s.owner.native.Find(ctx, r)
		if err != nil {
			return nil, err
		}
		if !found {
			return s.owner.c.logs.OpenStream(ctx, r.ID, runlog.ReadRequest{Selector: runlog.Selector{NodeID: q.NodeID, Container: q.Container}})
		}
		return s.owner.native.OpenLog(ctx, binding, entry, q.Follow)
	}
	return s.owner.c.logs.OpenStream(ctx, r.ID, runlog.ReadRequest{Selector: runlog.Selector{NodeID: q.NodeID, Container: q.Container}})
}
func (s serverStreams) Backup(ctx context.Context, c apicontract.Caller, _ apicontract.Empty) (io.ReadCloser, error) {
	// Authorize before publishing an asynchronous reader.
	if _, err := s.owner.service.PrepareAdminVersion(ctx, c, apicontract.AdminVersionArgs{}); err != nil {
		return nil, err
	}
	return backupStream(ctx, func(ctx context.Context, out io.WriteCloser) error {
		_, err := s.owner.service.Backup(ctx, c.Actor, out)
		return err
	}), nil
}

func backupStream(ctx context.Context, produce func(context.Context, io.WriteCloser) error) io.ReadCloser {
	life, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := produce(life, auditedBackupSink{writer})
		_ = writer.CloseWithError(err)
	}()
	return &joinedBackup{PipeReader: reader, cancel: cancel, done: done}
}

// The stream publishes EOF only after the service records its completion receipt.
type auditedBackupSink struct{ *io.PipeWriter }

func (auditedBackupSink) Close() error { return nil }

type joinedBackup struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   <-chan struct{}
	once   sync.Once
}

func (r *joinedBackup) Close() error {
	err := r.PipeReader.Close()
	r.once.Do(func() { r.cancel(); <-r.done })
	return err
}

func focusLogStep(r stateport.Run, steps []stepmodel.RunStep, selector logcontract.Selector) logcontract.Selector {
	var failed, running *string
	if r.Failure != nil {
		failed = &r.Failure.Step
	}
	for _, s := range steps {
		switch s.Status {
		case stepmodel.StepFailed:
			if failed == nil {
				failed = new(s.Path)
			}
		case stepmodel.StepRunning:
			if running == nil {
				running = new(s.Path)
			}
		}
	}
	return runlog.SelectStep(selector, failed, running).Selector
}
