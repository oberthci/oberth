package gitcache

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestPushAndAdmissionShareWorkflowPathRule(t *testing.T) {
	for _, file := range []string{".oberth/walk.yaml", "hack/walk-vm.yaml", "-x", "a b", "x=y", "x/.git/config", "../walk.yaml", "/walk.yaml", "./walk.yaml", "walk\n.yaml", strings.Repeat("x", 1025)} {
		t.Run(file, func(t *testing.T) {
			_, hookErr := ParsePushOptions([]string{"oberth.run=1", "oberth.file=" + file})
			_, _, admitErr := workflowpolicy.Select(nil, workflowpolicy.SelectionRequest{Repository: "origin/team/repo", Ref: "refs/heads/topic", File: file, Manual: true})
			if (hookErr == nil) != (admitErr == nil) {
				t.Fatal("hook and admission disagree on the selected path", hookErr, admitErr)
			}
		})
	}
}
