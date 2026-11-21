package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
)

func TestLocalDryRunRequiresPreparation(t *testing.T) {
	a, out, diagnostic := fixture(t, fake.Bindings())
	a.base = filepath.Join(t.TempDir(), "absent-config")
	code := a.run(t.Context(), []string{"install", "--chart", "../../charts/oberth", "--dry-run"})
	if code == 0 || !strings.Contains(diagnostic.String(), "Select an explicit kubeconfig context.") || out.Len() != 0 {
		t.Fatalf("local chart bypassed preparation: %d %s %s", code, out, diagnostic)
	}
}

func TestLocalDryRunShowsReviewedPlan(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		engine, charts := offlineInstallEngine(t, t.TempDir())
		flags := installationFlags("install")
		if err := flags.Parse([]string{"--context=chosen", "--chart=../../charts/oberth", "--dry-run", "--output=" + format}); err != nil {
			t.Fatal(err)
		}
		request := installationRequest{name: "install", flags: flags}
		session := installationSession{engine: engine, request: request, choices: request.choices()}
		var out bytes.Buffer
		if err := session.applyInstallation(t.Context(), strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Context:", "Namespaces:", "HTTPS:", "unverified local chart", installer.StoreSnapshotHint} {
			if !strings.Contains(out.String(), want) {
				t.Fatal("missing review", out.String())
			}
		}
		if format == "json" {
			assertLocalReviewImages(t, out.Bytes())
		}
		if strings.Contains(out.String(), "kind: Deployment") || strings.Contains(out.String(), "Thank you") || len(charts.Previews) != 1 || len(charts.Applies) != 0 {
			t.Fatal("dry run rendered or applied", out.String())
		}
	}
}

func assertLocalReviewImages(t *testing.T, output []byte) {
	t.Helper()
	var review struct {
		Images map[string]string `json:"images"`
	}
	want := map[string]string{"server": "oberthci/oberth:dev", "source": "oberthci/oberth-source-plugin:dev"}
	if err := json.Unmarshal(output, &review); err != nil || !reflect.DeepEqual(review.Images, want) {
		t.Fatal("JSON dry run omitted image references", err, string(output))
	}
}

func TestInstallationChoicesPreserveUnsetAndExplicitValues(t *testing.T) {
	for _, args := range [][]string{nil, {"--parallel", "0", "--no-link-local-filter=false", "--no-pod-defaults"}} {
		flags := installationFlags("install")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		choices := (installationRequest{flags: flags}).choices()
		if len(args) == 0 {
			if !reflect.ValueOf(choices).IsZero() {
				t.Fatal("unset choices became explicit overrides")
			}
		} else if choices.Parallel == nil || *choices.Parallel != 0 || choices.Egress == nil || !*choices.Egress || choices.PodFloor == nil || *choices.PodFloor {
			t.Fatal("explicit zero/false choices were lost", choices)
		}
	}
}

func TestSecretStepPreservesCommandArguments(t *testing.T) {
	flags := installationFlags("secretstore exec")
	args, err := installationArgs(installationCommand{name: "secretstore exec"}, flags, []string{"--", "tool", "--literal", "$(not-a-shell)", ""})
	if err != nil || len(args) != 4 || args[2] != "$(not-a-shell)" || args[3] != "" {
		t.Fatal("step argv was reparsed", args, err)
	}
}
