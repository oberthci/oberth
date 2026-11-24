package installer_test

import (
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/installer"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"slices"
	"strings"
	"testing"
)

func TestEffectiveSettingsUseExistingPodSecurityAndExplicitDefault(t *testing.T) {
	f := setup(t)
	pipelineNamespaceLevel(t, f, "restricted")
	parallel := 4
	f.choices.Parallel = &parallel
	p := prepare(t, f)
	detail := strings.Join(p.Detail(), "\n")
	if !strings.Contains(detail, `"statusPodSecurity": "restricted"`) || !strings.Contains(detail, `"parallel runs"`) {
		t.Fatal("effective security level or explicit default origin lost", detail)
	}
}

func TestEffectivePodSecurityCannotMoveAfterReview(t *testing.T) {
	f := setup(t)
	pipelineNamespaceLevel(t, f, "baseline")
	p := prepare(t, f)
	namespace, err := f.client.CoreV1().Namespaces().Get(t.Context(), "oberth-runs", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	namespace.Labels["pod-security.kubernetes.io/enforce"] = "restricted"
	if _, err = f.client.CoreV1().Namespaces().Update(t.Context(), namespace, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" || len(f.charts.Applies) != 0 {
		t.Fatal("changed security level applied", err)
	}
}

func TestExplicitScalarDefaultsKeepChosenOrigins(t *testing.T) {
	f := setup(t)
	f.choices.Namespace, f.choices.PipelineNamespace = "oberth", "oberth-runs"
	f.choices.HTTPSNodePort, f.choices.SSHNodePort, f.choices.SSHPort = 30443, 30022, 30022
	f.choices.ClearSourcePins = true
	f.choices.Values = map[string]any{"secretStore": map[string]any{"bundled": true}}
	p := prepare(t, f)
	var detail struct {
		Values struct {
			Chosen []string `json:"statusChosen"`
		} `json:"values"`
	}
	if err := json.Unmarshal([]byte(strings.Join(p.Detail(), "\n")), &detail); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"namespace", "pipeline namespace", "HTTPS node port", "SSH node port", "SSH port", "source pins", "store mode"} {
		if !slices.Contains(detail.Values.Chosen, name) {
			t.Fatal("explicit default origin lost", name, detail.Values.Chosen)
		}
	}
}
