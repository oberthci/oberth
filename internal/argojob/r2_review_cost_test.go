//go:build linux

package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestReviewR1SourceTrustCPUWithMillionCount(t *testing.T) {
	e := argojob.New(argojob.Config{})
	w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{Env: []core.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: "1000000"}}}}}}}
	in := admissioncontract.TransformInput{SourceBindings: []sourcecontract.Binding{{Template: "build", Descriptor: sourcecontract.Descriptor{Kind: sourcecontract.Main}}}}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var before, after syscall.Rusage
	compositionOK(t, syscall.Getrusage(syscall.RUSAGE_THREAD, &before))
	compositionOK(t, e.Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), w, in))
	compositionOK(t, syscall.Getrusage(syscall.RUSAGE_THREAD, &after))
	cpu := time.Duration(after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano())
	t.Logf("env_entries=1 authored_count=1000000 thread_cpu=%s", cpu)
	if cpu > 20*time.Millisecond {
		t.Fatalf("tiny environment consumed %s CPU scanning absent count slots", cpu)
	}
}
