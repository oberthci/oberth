package agent_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestSkillPushOptionsMatchReceiver(t *testing.T) {
	text, err := os.ReadFile("../internal/api/skills/oberth-ci/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	options := regexp.MustCompile("-o ([^\\s`;]+)").FindAllSubmatch(text, -1)
	if len(options) == 0 {
		t.Fatal("skill omits its CI push option")
	}
	for _, option := range options {
		got, err := gitcache.ParsePushOptions([]string{string(option[1])})
		if err != nil || !got.Run {
			t.Fatalf("skill CI push option %s is not accepted: %v", option[1], err)
		}
	}
}
