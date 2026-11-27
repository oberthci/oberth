package gitcache

import (
	"os"
	"regexp"
	"testing"
)

func TestSkillPushOptionsMatchReceiver(t *testing.T) {
	text, err := os.ReadFile("../api/skills/oberth-ci/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	options := regexp.MustCompile("-o ([^\\s`;]+)").FindAllSubmatch(text, -1)
	if len(options) == 0 {
		t.Fatal("skill omits its CI push option")
	}
	for _, option := range options {
		got, err := ParsePushOptions([]string{string(option[1])})
		if err != nil || !got.Run {
			t.Fatalf("skill CI push option %s is not accepted: %v", option[1], err)
		}
	}
}
