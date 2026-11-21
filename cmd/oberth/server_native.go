package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argowatch"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/remotecommand"
)

func (o *serverOwners) nativePart(ctx context.Context) (Part, error) {
	ns := o.c.config.PipelineNamespace
	workflows := o.cluster.Argo.ArgoprojV1alpha1().Workflows(ns)
	pods := o.cluster.Kube.CoreV1().Pods(ns)
	hub, err := argowatch.New(argowatch.Config{Namespace: ns, Workflows: workflows, Pods: pods})
	if err != nil {
		return Part{}, err
	}
	o.c.watch = hub.Run
	o.c.health.watch = hub.Healthy
	metrics := &argojob.Metrics{}
	o.c.health.nativeIncomplete = metrics.IncompleteEvidence.Load
	o.native = argojob.New(argojob.Config{InstanceID: o.c.config.Namespace, SecretStore: o.c.delivery, Children: scheduleChildren{o.c}, ChildRuns: o.c.store, Metrics: metrics, Workflows: argojob.WorkflowClient{NativeWorkflows: workflows}, Accounts: o.cluster.Kube.CoreV1().ServiceAccounts(ns), Pods: nativePods{PodInterface: pods, owner: o}, Runs: o.c.store, Progress: o.c.store, Reporter: nativeReporter{o}, Logs: o.c.logs, Cache: hub, Now: o.c.clock.Now, Logger: slog.New(slog.NewTextHandler(o.c.diagnostic, nil))})
	return Part{Name: "native execution", Run: o.native.Run}, nil
}

type nativePods struct {
	typed.PodInterface
	owner *serverOwners
}

func (p nativePods) Logs(ctx context.Context, name string, opts core.PodLogOptions) (io.ReadCloser, error) {
	return p.PodInterface.GetLogs(name, &opts).Stream(ctx)
}
func (p nativePods) Exec(ctx context.Context, r argojob.ExecRequest) error {
	c := p.owner
	request := c.cluster.Kube.CoreV1().RESTClient().Post().Namespace(c.c.config.PipelineNamespace).Resource("pods").Name(r.Pod).SubResource("exec").VersionedParams(&core.PodExecOptions{Container: r.Container, Command: r.Command, Stdin: r.In != nil, Stdout: r.Out != nil, Stderr: r.Err != nil}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(c.cluster.REST, "POST", request.URL())
	if err != nil {
		return err
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: r.In, Stdout: r.Out, Stderr: r.Err})
}

type nativeReporter struct{ owner *serverOwners }

func (r nativeReporter) Finished(ctx context.Context, id string, o argojob.Observation) error {
	return r.owner.service.Finished(ctx, service.FinishedRun{RunID: id, Status: o.Status, NativePhase: &o.Phase, Reason: o.Reason, Detail: o.Detail, Failure: o.Failure})
}
func (r nativeReporter) Held(ctx context.Context, h model.HeldNotice) error {
	return r.owner.service.Held(ctx, h)
}
