package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestDocumentedRemovalSelectorsMatchNativeRuns(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\n[ \"$1 $2 $3 $4 $5 $7\" = '--context chosen -n retained delete -l' ] || exit 1\nprintf '%s %s\\n' \"$6\" \"$8\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	section := installDocSection(t, "## Remove an installation")
	command := exec.Command("sh", "-ec", removalCleanup(t, section, "Retained pipeline namespace"))
	command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "context=chosen", "namespace=frontdoor", "pipeline_namespace=retained")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	fixtures := map[string]string{"workflows.argoproj.io": "workflow-payload.json", "cronworkflows.argoproj.io": "cron-workflow-payload.json"}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		kind, selector, _ := strings.Cut(line, " ")
		if kind == "serviceaccounts" {
			continue
		}
		file, ok := fixtures[kind]
		if !ok {
			t.Fatal("unexpected run resource", kind)
		}
		checkRemovalSelector(t, file, selector)
		delete(fixtures, kind)
	}
	if len(fixtures) != 0 {
		t.Fatal("run resources left behind", fixtures)
	}
}

func checkRemovalSelector(t *testing.T, fixture, raw string) {
	t.Helper()
	data, err := os.ReadFile("../../internal/installer/testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	var object struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	selector, err := labels.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !selector.Matches(labels.Set(object.Metadata.Labels)) {
		t.Fatal("leaves native run behind", fixture, raw)
	}
	object.Metadata.Labels["workflows.argoproj.io/controller-instanceid"] = "other"
	if selector.Matches(labels.Set(object.Metadata.Labels)) {
		t.Fatal("selects another installation", fixture, raw)
	}
}
