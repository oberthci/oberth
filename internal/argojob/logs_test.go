package argojob_test

import (
	"errors"
	"io"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	core "k8s.io/api/core/v1"
)

func TestLogBinding(t *testing.T) {
	t.Parallel()
	_, f, b := submitted(t)
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name + ".build"
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	p := nativePod(f.Object, n, 7)
	pods := &fake.Pods{Object: p}
	e := argojob.New(argojob.Config{Workflows: f, Pods: pods})
	entry := runlog.ContainerLog{NodeID: n.ID, PodUID: string(p.UID), PodName: p.Name, Namespace: p.Namespace, Container: "main"}
	stream, err := e.OpenLog(t.Context(), b, entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, stream); err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	pods.OnLogs = func() { pods.Object.UID = "replacement" }
	if _, err = e.OpenLog(t.Context(), b, entry, false); err == nil {
		t.Fatal("replacement after open accepted")
	}
	pods.OnLogs = nil
	if _, err = e.OpenLog(t.Context(), b, entry, false); err == nil || pods.Opens != 2 {
		t.Fatal("replacement before open accepted")
	}
}

func TestLogsRequireDeclaredContainer(t *testing.T) {
	t.Parallel()
	for _, container := range []string{"main", "init", "not-declared"} {
		t.Run(container, func(t *testing.T) {
			_, f, b := submitted(t)
			n := nativeNode("one", ".build", wf.NodeFailed, 1)
			n.Name = b.Workflow.Name + ".build"
			f.Object.Status.Nodes = wf.Nodes{n.ID: n}
			p := nativePod(f.Object, n, 7)
			p.Spec.InitContainers = []core.Container{{Name: "init"}}
			pods := &fake.Pods{Object: p}
			e := argojob.New(argojob.Config{Workflows: f, Pods: pods})
			entry := runlog.ContainerLog{NodeID: n.ID, PodUID: string(p.UID), PodName: p.Name, Namespace: p.Namespace, Container: container}
			stream, err := e.OpenLog(t.Context(), b, entry, false)
			if stream != nil {
				defer func() { compositionOK(t, stream.Close()) }()
			}
			if container == "not-declared" {
				if !errors.As(err, new(*nativebinding.Refusal)) || pods.Opens != 0 || stream != nil {
					t.Fatal("undeclared container log opened", err, pods.Opens)
				}
				return
			}
			if err != nil || pods.Opens != 1 || stream == nil {
				t.Fatal("declared container log refused", err, pods.Opens)
			}
		})
	}
}
func TestLogsRefuseMultipleControllersAndMismatchedNodeName(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"controllers", "node name"} {
		t.Run(field, func(t *testing.T) {
			_, f, b := submitted(t)
			n := nativeNode("one", ".build", wf.NodeFailed, 1)
			n.Name = b.Workflow.Name + ".build"
			f.Object.Status.Nodes = wf.Nodes{n.ID: n}
			p := nativePod(f.Object, n, 7)
			if field == "controllers" {
				p.OwnerReferences = append(p.OwnerReferences, p.OwnerReferences[0])
			} else {
				p.Annotations["workflows.argoproj.io/node-name"] = "other"
			}
			pods := &fake.Pods{Object: p}
			e := argojob.New(argojob.Config{Workflows: f, Pods: pods})
			entry := runlog.ContainerLog{NodeID: n.ID, PodUID: string(p.UID), PodName: p.Name, Namespace: p.Namespace, Container: "main"}
			if _, err := e.OpenLog(t.Context(), b, entry, false); err == nil || pods.Opens != 0 {
				t.Fatal("unbound log opened", err)
			}
		})
	}
}
