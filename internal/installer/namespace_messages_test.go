package installer_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/installer"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestForeignReleaseMessagesDescribeTheirOwner(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"secret", "manifest", "namespace"} {
		for _, recheck := range []bool{false, true} {
			t.Run(shape+map[bool]string{true: "/recheck"}[recheck], func(t *testing.T) { foreignMessageCase(t, shape, recheck) })
		}
	}
}

func foreignMessageCase(t *testing.T, shape string, recheck bool) {
	t.Helper()
	f := setup(t)
	var plan *installer.Plan
	if recheck {
		plan = prepare(t, f)
	}
	record := foreignNamespaceRelease()
	record.Manifest = "kind: Namespace\nmetadata:\n  name: oberth-runs\n"
	if shape == "manifest" {
		record.Manifest = "metadata: ["
	}
	saveForeignRelease(t, f, record)
	if shape == "secret" {
		port := f.client.CoreV1().Secrets(record.Namespace)
		secret, err := port.Get(t.Context(), "sh.helm.release.v1.fixture-owner.v1", meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		secret.Data["release"] = []byte("unreadable")
		if _, err := port.Update(t.Context(), secret, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f.client.ClearActions()
	var err error
	if recheck {
		_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	} else {
		_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
	}
	assertForeignMessage(t, shape, err)
	assertNoInstallWrites(t, f)
}

func TestMembershipAndAnswerMessageBudgets(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"release-membership", "namespace-owner", "install-answers", "administrator-retained", "installation-owner"} {
		p := installer.NewProblem(code, nil, "")
		if p.Reason == "" || utf8.RuneCountInString(p.Reason) > 80 || p.Remedy == "" || utf8.RuneCountInString(p.Remedy) > 120 || strings.Contains(p.Reason+p.Remedy, "…") {
			t.Fatal("message budget", code, p)
		}
		if (code == "administrator-retained" || code == "installation-owner") && !strings.Contains(p.Remedy, "choose another --namespace and unused --https-node-port and --ssh-node-port") {
			t.Fatal("ownership remedy must change the namespace and port values", code, p)
		}
		t.Logf("%s: refusal %d, remedy %d: %s / %s", code, utf8.RuneCountInString(p.Reason), utf8.RuneCountInString(p.Remedy), p.Reason, p.Remedy)
	}
}

func assertForeignMessage(t *testing.T, shape string, err error) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "Helm release") || strings.Contains(refusal.Reason, "installation") || strings.Contains(refusal.Remedy, "database") || !strings.Contains(refusal.Detail, "control") {
		t.Fatal("foreign release got installation guidance", err)
	}
	if shape == "namespace" && !strings.Contains(refusal.Detail, "control/fixture-owner") {
		t.Fatal("holder missing", err)
	}
}
