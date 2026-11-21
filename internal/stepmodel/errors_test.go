package stepmodel

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestInvalidStatusAlwaysExplainsReasonAndRemedy(t *testing.T) {
	err := &InvalidStatusError{}
	for text, limit := range map[string]int{err.Reason(): 80, err.Remedy(): 120} {
		if problem := publicerr.CheckMessage(text, limit); problem != nil {
			t.Fatal(problem, text)
		}
	}
	if err.Reason() == "" || err.Remedy() == "" || err.Error() == "" {
		t.Fatal("incomplete refusal")
	}
}
