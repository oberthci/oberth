package setuptui

import (
	"bytes"
	"strings"
	"testing"
)

func TestCopyValuesShareOneFormatterWithReview(t *testing.T) {
	for _, value := range []string{"/profile/" + strings.Repeat("long", 30), "ssh-ed25519 " + strings.Repeat("A", 100), "https://oberth.ci/" + strings.Repeat("path/", 25), "oberth repo add ssh://forge.oberth.ci/team/widget.git --upstream origin"} {
		var out bytes.Buffer
		if err := writeLine(&out, "Value: "+value); err != nil || out.String() != "Value:\n"+value+"\n" {
			t.Fatal("copy value was wrapped", out.String(), err)
		}
	}
	if CopyableLines("Prose: use complete words when explaining the next action.") != nil {
		t.Fatal("ordinary prose classified as a copy value")
	}
}
