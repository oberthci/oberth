package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	core "k8s.io/api/core/v1"

	"strings"
	"testing"
)

func TestAccountsRequireUIDAndNamePublicContext(t *testing.T) {
	t.Parallel()
	want := account(t, selectionKind(false, false), "")
	for _, names := range []map[string]string{{want: ""}, {}} {
		e := argojob.New(argojob.Config{Accounts: fake.Accounts{Names: names}})
		w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{}}}}}
		err := e.Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), w, admissioncontract.TransformInput{Mode: admissioncontract.ModeSubmit})
		var public *publicerr.Error
		if !errors.As(err, &public) || public.Reason() != "CI account for the step is missing" || !strings.Contains(public.Detail, "build") || !strings.Contains(public.Detail, want) {
			t.Fatal("missing account details", err)
		}
	}
}
func TestSourceTrustIndexesActualEntriesAndKeepsOtherValues(t *testing.T) {
	t.Parallel()
	for _, count := range []string{"1", "1000000000000000000000000000000000000000000"} {
		w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{Env: []core.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: count}, {Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"}, {Name: "GIT_CONFIG_VALUE_0", Value: "/somewhere"}}}}}}}
		h := argojob.New(argojob.Config{}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo"})
		in := admissioncontract.TransformInput{SourceBindings: []sourcecontract.Binding{{Template: "build", Descriptor: sourcecontract.Descriptor{Kind: sourcecontract.Main}}}}
		compositionOK(t, h.TransformNative(t.Context(), w, in))
		env := w.Spec.Templates[0].Container.Env
		wantCount := "1000000000000000000000000000000000000000001"
		if count == "1" {
			wantCount = "2"
		}
		if len(env) != 5 || env[0].Value != wantCount || env[3].Name != "GIT_CONFIG_KEY_"+count || env[4].Value != "/work/src" {
			t.Fatal(env)
		}
		compositionOK(t, h.TransformNative(t.Context(), w, in))
		if len(w.Spec.Templates[0].Container.Env) != 5 {
			t.Fatal("not idempotent")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := argojob.New(argojob.Config{}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(ctx, &wf.Workflow{}, admissioncontract.TransformInput{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestResourceAccountAppliesOnlyToResourceLeaves(t *testing.T) {
	t.Parallel()
	w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{}}, {Name: "deploy", Resource: &wf.ResourceTemplate{}}}}}
	in := admissioncontract.TransformInput{Identity: admissioncontract.IdentityEvidence{Resource: &admissioncontract.AccountEvidence{Name: "capability", UID: "capability-uid"}}}
	compositionOK(t, argojob.New(argojob.Config{}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), w, in))
	if w.Spec.Templates[0].ServiceAccountName != account(t, selectionKind(false, false), "") || w.Spec.Templates[1].ServiceAccountName != "capability" {
		t.Fatal(w.Spec.Templates)
	}
}
func TestNegativeSourceTrustCountStartsAtZero(t *testing.T) {
	t.Parallel()
	w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{Env: []core.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: "-7"}}}}}}}
	in := admissioncontract.TransformInput{SourceBindings: []sourcecontract.Binding{{Template: "build", Descriptor: sourcecontract.Descriptor{Kind: sourcecontract.Main}}}}
	compositionOK(t, argojob.New(argojob.Config{}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), w, in))
	env := w.Spec.Templates[0].Container.Env
	if len(env) != 3 || env[0].Value != "1" || env[1].Name != "GIT_CONFIG_KEY_0" || env[2].Name != "GIT_CONFIG_VALUE_0" {
		t.Fatal(env)
	}
}
