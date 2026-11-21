package setuptui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCanceledAskLeavesInputAndOutputUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	input := bytes.NewBufferString("chosen\nhttps://oberth.local\n")
	size := input.Len()
	var out bytes.Buffer
	choices, err := Ask(ctx, bufio.NewScanner(input), &out, []string{"chosen"})
	if !errors.Is(err, context.Canceled) || out.Len() != 0 || input.Len() != size || choices.Context != "" || choices.URL != "" {
		t.Fatal("canceled selection produced prompts, consumed answers or selected a target", choices, err)
	}
}
