package installer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	installfake "github.com/oberthci/oberth/internal/installer/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ktesting "k8s.io/client-go/testing"
)

func TestNodePortsValidatedByAPIBeforeWrites(t *testing.T) {
	for _, tc := range []nodePortAPICase{
		{"out of range", "provided port is not in the valid range", 443},
		{"allocated outside inventory", "provided port is already allocated", 31443},
		{"custom range", "", 24000},
	} {
		for _, existing := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: " existing namespace"}[existing], func(t *testing.T) {
				nodePortValidationCase(t, tc, existing)
			})
		}
	}
}

func TestNodePortAPIRevalidationRefusesBeforeWrites(t *testing.T) {
	f := setup(t)
	p := prepare(t, f)
	f.client.PrependReactor("create", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("port allocation changed")
	})
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	if err == nil || !strings.Contains(err.Error(), "port allocation changed") {
		t.Fatal(err)
	}
	assertNoInstallWrites(t, f)
}

func TestOwnedServicePortValidationUsesDryRunUpdate(t *testing.T) {
	f := setup(t)
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth", ResourceVersion: "7", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}}
	if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.CoreV1().Services("oberth").Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	called := false
	f.client.PrependReactor("update", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
		called = true
		if !installfake.DryRunService(a) {
			t.Fatal(a)
		}
		return true, a.(ktesting.UpdateAction).GetObject(), nil
	})
	f.client.ClearActions()
	prepare(t, f)
	if !called {
		t.Fatal("owned ports were not validated by update")
	}
	assertNoInstallWrites(t, f)
	f.client.PrependReactor("update", "services", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "oberth", errors.New("update denied"))
	})
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "service-update" {
		t.Fatal(err)
	}
	assertNoInstallWrites(t, f)
}

type nodePortAPICase struct {
	name, detail string
	port         int
}

func nodePortValidationCase(t *testing.T, tc nodePortAPICase, existing bool) {
	f := setup(t)
	f.choices.HTTPSNodePort = tc.port
	if existing {
		if _, err := f.client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	f.client.PrependReactor("create", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
		calls++
		s := checkServiceDryRun(t, a, tc.port, existing)
		if tc.detail != "" {
			return true, nil, errors.New(tc.detail)
		}
		return true, s, nil
	})
	f.client.ClearActions()
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if tc.detail != "" {
		if err == nil || !strings.Contains(err.Error(), tc.detail) {
			t.Fatal("API refusal lost", err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		p.Close()
	}
	if calls != 1 {
		t.Fatal("API validation missing", calls)
	}
	assertNoInstallWrites(t, f)
}

func checkServiceDryRun(t *testing.T, a ktesting.Action, port int, existing bool) *corev1.Service {
	t.Helper()
	if !installfake.DryRunService(a) {
		t.Fatal("validation was not dryRun=All")
	}
	wantNS := "default"
	if existing {
		wantNS = "oberth"
	}
	if a.GetNamespace() != wantNS {
		t.Fatal(a.GetNamespace())
	}
	s := a.(ktesting.CreateAction).GetObject().(*corev1.Service)
	if len(s.Spec.Ports) != 2 || int(s.Spec.Ports[0].NodePort) != port || s.Spec.Ports[1].NodePort != 30022 {
		t.Fatal(s.Spec.Ports)
	}
	return s
}

func TestServiceDryRunRefusalExplainsCause(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		cause      error
	}{
		{"permission", "service-create", apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "oberth-port-check", errors.New("permission denied in default"))},
		{"webhook", "cluster", errors.New("webhook does not support dry run")},
		{"unrelated invalid field", "cluster", apierrors.NewInvalid(schema.GroupKind{Kind: "Service"}, "probe", field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), "probe", "policy")})},
		{"port range", "node-port-api", apierrors.NewInvalid(schema.GroupKind{Kind: "Service"}, "probe", field.ErrorList{field.Invalid(field.NewPath("spec", "ports").Index(0).Child("nodePort"), 443, "outside the configured range")})},
		{"allocated port", "node-port-api", apierrors.NewInvalid(schema.GroupKind{Kind: "Service"}, "probe", field.ErrorList{field.Invalid(field.NewPath("spec", "ports").Index(1).Child("nodePort"), 30022, "provided port is already allocated")})},
	} {
		for _, recheck := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: " recheck"}[recheck], func(t *testing.T) {
				f := setup(t)
				var plan *installer.Plan
				if recheck {
					plan = prepare(t, f)
				}
				f.client.PrependReactor("create", "services", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, tc.cause })
				var err error
				if recheck {
					_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
				} else {
					_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
				}
				var problem *installer.Problem
				if !errors.As(err, &problem) || problem.Code != tc.code || !strings.Contains(problem.Detail, tc.cause.Error()) {
					t.Fatal(err)
				}
				assertNoInstallWrites(t, f)
			})
		}
	}
}
