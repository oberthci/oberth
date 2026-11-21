package installer

import (
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestSubmittedPayloadsSelectOnlyTheirController(t *testing.T) {
	t.Parallel()
	files, err := fs.Glob(definitionPayloads, "testdata/*-payload.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := definitionPayloads.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var object struct {
			Metadata meta.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if !isolatedSubmission(&object.Metadata) {
			t.Error("unlabelled or wrongly selected submission", name)
		}
	}
}

func isolatedSubmission(object meta.Object) bool {
	for _, id := range []string{"", "another-server", "frontdoor"} {
		options := meta.ListOptions{}
		instanceid.NewService(id).With(&options)
		selector, err := labels.Parse(options.LabelSelector)
		if err != nil || selector.Matches(labels.Set(object.GetLabels())) != (id == "frontdoor") {
			return false
		}
	}
	return true
}

func TestSubmissionIsolationGuardRejectsUnlabelledObject(t *testing.T) {
	t.Parallel()
	if isolatedSubmission(&meta.ObjectMeta{}) {
		t.Fatal("unlabelled object escaped submission coverage")
	}
}
