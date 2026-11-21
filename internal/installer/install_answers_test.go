package installer_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestInterruptedFirstInstallRetainsAnswers(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"first mutation", "before revision", "revision is pending"} {
		t.Run(stage, func(t *testing.T) {
			interruptedAnswersCase(t, stage)
		})
	}
}

func TestPendingInstallAnswersMustMatchLocalRecord(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"namespace", "https", "ssh"} {
		t.Run(field, func(t *testing.T) {
			pendingAnswersCase(t, field)
		})
	}
}

func interruptedAnswersCase(t *testing.T, stage string) {
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	foreignService(t, f, 30443)
	service, err := f.client.CoreV1().Services("existing").Get(t.Context(), "front", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service.Spec.Ports = append(service.Spec.Ports, corev1.ServicePort{Port: 22, NodePort: 30022})
	if _, err = f.client.CoreV1().Services("existing").Update(t.Context(), service, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := prepare(t, f)
	stop := errors.New("interrupted")
	if stage == "first mutation" {
		f.client.PrependReactor("create", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, stop })
	}
	_, err = f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if phase == installer.Installing {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if stage == "revision is pending" {
		f.charts.History.Latest = installer.Revision{Number: 1, Kind: "local", Status: "pending-install", Version: "unset", Values: f.charts.Previews[0].Values}
	}
	f.choices = installer.Choices{Context: "test"}
	next := prepare(t, f)
	review := strings.Join(next.Review(), "\n")
	for _, want := range []string{"Namespaces: second / jobs", "node port 31443", "node port 31022"} {
		if !strings.Contains(review, want) {
			t.Fatal("retry discarded answer", review)
		}
	}
}

func pendingAnswersCase(t *testing.T, field string) {
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	p := prepare(t, f)
	stop := errors.New("interrupted")
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if phase == installer.Installing {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	values := f.charts.Previews[0].Values
	if field == "namespace" {
		values["pipeline"].(map[string]any)[field] = "changed"
	} else {
		values["nodePorts"].(map[string]any)[field] = 32001
	}
	f.charts.History.Latest = installer.Revision{Number: 1, Kind: "local", Status: "pending-install", Version: "unset", Values: values}
	before, err := os.ReadFile(filepath.Join(f.dir, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	_, err = f.engine.Prepare(t.Context(), f.release, installer.Choices{Context: "test"})
	assertPendingAnswerMessage(t, err)
	if field == "ssh" {
		values["nodePorts"].(map[string]any)["ssh"] = f.choices.SSHNodePort
		_, err = f.engine.Prepare(t.Context(), f.release, installer.Choices{Context: "test", SSHNodePort: f.choices.SSHNodePort + 1})
		assertPendingAnswerMessage(t, err)
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "installation.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("refusal changed profile", err)
	}
	for _, a := range f.client.Actions() {
		if !readOnly(a) {
			t.Fatal("refusal wrote", a)
		}
	}
}

func assertPendingAnswerMessage(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "answers differ") || !strings.Contains(err.Error(), "revision is pending") || strings.Contains(err.Error(), "after removal") {
		t.Fatal(err)
	}
}
