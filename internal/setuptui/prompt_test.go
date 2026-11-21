package setuptui

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAskUsesExplicitContext(t *testing.T) {
	var out bytes.Buffer
	choices, err := Ask(context.Background(), bufio.NewScanner(strings.NewReader("dev\nhttps://localhost:34443\n")), &out, []string{"dev"})
	if err != nil || choices.Context != "dev" || choices.URL != "https://localhost:34443" {
		t.Fatal(choices, err)
	}
	if _, err = Ask(context.Background(), bufio.NewScanner(strings.NewReader("wrong\n\n")), &out, []string{"dev"}); err == nil {
		t.Fatal("unknown context accepted")
	}
}
func TestReviewLetsTerminalWrapWholePathsAndUnicode(t *testing.T) {
	for _, text := range []string{
		"Profile: /private/" + strings.Repeat("directory/", 20) + "installation.json",
		"Context: " + strings.Repeat("å", 172) + " complete detail remains available",
		strings.Repeat("界", 81) + strings.Repeat("e\u0301", 30),
	} {
		var out bytes.Buffer
		if err := writeLine(&out, text); err != nil {
			t.Fatal(err)
		}
		if out.String() != strings.Replace(text, "Profile: ", "Profile:\n", 1)+"\n" {
			t.Fatal("hard wrapped review text", out.String())
		}
	}
}

func TestReviewRowBudgetCountsWideCharacters(t *testing.T) {
	text := strings.Repeat("界", 81) + strings.Repeat("e\u0301", 30)
	rows := 0
	for remaining := text; remaining != ""; rows++ {
		remaining = remaining[lineBreak(remaining, 80):]
	}
	if rows != 3 {
		t.Fatal("wrong terminal row budget", rows)
	}
}
