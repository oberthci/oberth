package installer_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestExplicitAnswersReplaceRecordWithoutRevisions(t *testing.T) {
	t.Parallel()
	for _, incomplete := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed record", true: "answers only"}[incomplete], func(t *testing.T) {
			replaceRecordCase(t, incomplete)
		})
	}
}

func replaceRecordCase(t *testing.T, incomplete bool) {
	t.Helper()
	f := recordedWithoutRevisions(t, incomplete)
	before := installationRecord(t, f)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	changes := []string{"HTTPS node port: 30443 -> 31443", "SSH node port: 30022 -> 31022"}
	if !incomplete {
		f.choices.Namespace, f.choices.PipelineNamespace = "oberth", "oberth-runs"
	} else {
		changes = append(changes, "Server namespace: oberth -> second", "Pipeline namespace: oberth-runs -> jobs")
	}
	plan := prepare(t, f)
	for _, change := range changes {
		if !strings.Contains(strings.Join(plan.Review(), "\n"), change) {
			t.Fatal("review omitted replacement", change, plan.Review())
		}
	}
	if installationRecord(t, f) != before {
		t.Fatal("review wrote the record")
	}
	result := apply(t, f, plan)
	if result.Profile.Namespace != f.choices.Namespace || result.Profile.PipelineNamespace != f.choices.PipelineNamespace || result.Profile.HTTPSNodePort != 31443 || result.Profile.SSHNodePort != 31022 {
		t.Fatal("replacement lost", result.Profile.Namespace, result.Profile.PipelineNamespace)
	}
	f.choices = installer.Choices{Context: "test"}
	if !strings.Contains(strings.Join(prepare(t, f).Review(), "\n"), "Namespaces: "+result.Profile.Namespace+" / "+result.Profile.PipelineNamespace) {
		t.Fatal("new record not restored")
	}
}

func TestRecordRestoresAnswersWithoutRevisions(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	apply(t, f, prepare(t, f))
	f.charts.History = installer.History{}
	f.choices = installer.Choices{Context: "test"}
	before := installationRecord(t, f)
	review := strings.Join(prepare(t, f).Review(), "\n")
	for _, expected := range []string{"Namespaces: second / jobs", "node port 31443", "node port 31022"} {
		if !strings.Contains(review, expected) {
			t.Fatal(review)
		}
	}
	if installationRecord(t, f) != before {
		t.Fatal("review changed the record")
	}
}

func recordedWithoutRevisions(t *testing.T, incomplete bool) fixture {
	t.Helper()
	f := setup(t)
	if !incomplete {
		apply(t, f, prepare(t, f))
		f.charts.History = installer.History{}
		return f
	}
	fail := true
	stop := errors.New("interrupted before mutation")
	f.client.PrependReactor("create", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) { return fail, nil, stop })
	plan := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	fail = false
	return f
}

func installationRecord(t *testing.T, f fixture) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAnyExistingRevisionKeepsRecordedAnswersBinding(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"pending-install", "pending-upgrade", "pending-rollback", "failed", "uninstalling", "uninstalled"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			apply(t, f, prepare(t, f))
			f.charts.History = installer.History{}
			f.charts.History.Latest = installer.Revision{Number: 1, Status: state, Kind: "local", Version: "unset", Values: f.charts.Previews[0].Values}
			before := installationRecord(t, f)
			f.choices.HTTPSNodePort = 31443
			f.client.ClearActions()
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			assertPendingAnswerMessage(t, err)
			if installationRecord(t, f) != before {
				t.Fatal("refusal changed record")
			}
			for _, action := range f.client.Actions() {
				if !readOnly(action) {
					t.Fatal("refusal wrote", action)
				}
			}
		})
	}
}

func TestIncompleteRecordNamespaceReplacementSurvivesInterruption(t *testing.T) {
	t.Parallel()
	f := recordedWithoutRevisions(t, true)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	plan := prepare(t, f)
	review := strings.Join(plan.Review(), "\n")
	if !strings.Contains(review, "Server namespace: oberth -> second") || !strings.Contains(review, "Pipeline namespace: oberth-runs -> jobs") {
		t.Fatal(review)
	}
	stop := errors.New("interrupted at first mutation")
	f.client.PrependReactor("create", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, stop })
	if _, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	f.choices = installer.Choices{Context: "test"}
	next := prepare(t, f)
	if !strings.Contains(strings.Join(next.Review(), "\n"), "Namespaces: second / jobs") {
		t.Fatal("replacement not recorded before mutation", next.Review())
	}
}

func TestCompletedRecordCannotReuseTrustInAnotherNamespace(t *testing.T) {
	t.Parallel()
	f := recordedWithoutRevisions(t, false)
	f.choices = installer.Choices{Context: "test", Namespace: "second", PipelineNamespace: "jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
	before := directoryBytes(t, f.dir)
	f.client.ClearActions()
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "profile-ca" {
		t.Fatal("completed profile reused for another installation", err)
	}
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) {
		t.Fatal("refusal changed the profile")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("refusal wrote cluster state", action)
		}
	}
}

func TestPendingUpgradeAnswersMustMatchCompletedRecord(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History.Latest.Status = "pending-upgrade"
	f.charts.History.Latest.Number++
	f.charts.History.Latest.Values["nodePorts"].(map[string]any)["https"] = 31443
	before := installationRecord(t, f)
	f.client.ClearActions()
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if err == nil || !strings.Contains(err.Error(), "answers differ") {
		t.Fatal(err)
	}
	if installationRecord(t, f) != before {
		t.Fatal("refusal changed record")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("refusal wrote", action)
		}
	}
}
