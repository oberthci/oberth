package setuptui

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestAskEnterSelectsShownContext(t *testing.T) {
	var out bytes.Buffer
	got, err := Ask(t.Context(), bufio.NewScanner(strings.NewReader("\n\n")), &out, []string{"dev"})
	if err != nil || got.Context != "dev" || !strings.Contains(out.String(), "Context [dev]:") {
		t.Fatal(got, err, out.String())
	}
}

func TestAskMultipleContextsRequireChoiceBeforeURL(t *testing.T) {
	var out bytes.Buffer
	_, err := Ask(t.Context(), bufio.NewScanner(strings.NewReader("\n")), &out, []string{"dev", "prod"})
	if err == nil || strings.Contains(out.String(), "Server URL") || !strings.Contains(out.String(), "choose one: dev, prod") {
		t.Fatal(err, out.String())
	}
}
