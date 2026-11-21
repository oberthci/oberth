package publicerr

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMessageLimitsAndSentenceBoundaries(t *testing.T) {
	for _, limit := range []int{40, 80, 120} {
		for _, text := range []string{"", "A complete message.", strings.Repeat("é", limit), "Use a .git file."} {
			if err := CheckMessage(text, limit); err != nil {
				t.Fatal(err)
			}
		}
		for _, text := range []string{strings.Repeat("é", limit+1), "Waiting...", "Waiting… ", "One. Two.", "One! Two.", "One? Two.", "One\nline"} {
			if CheckMessage(text, limit) == nil {
				t.Fatalf("accepted %q at %d", text, limit)
			}
		}
	}
}

func TestConstructorRejectsUnboundedOrMultipleSentenceDiagnostics(t *testing.T) {
	for _, pair := range [][2]string{{strings.Repeat("a", 81), "Retry."}, {"Failed.", strings.Repeat("a", 121)}, {"Failed. Again.", "Retry."}, {"Failed.", "Retry..."}} {
		t.Run(pair[0]+pair[1], func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("invalid message accepted")
				}
			}()
			_ = New(InvalidInput, pair[0], pair[1], nil)
		})
	}
}

func TestJoinedPublicDiagnosticsKeepBoundedSummariesAndFullDetail(t *testing.T) {
	var failures []error
	for i := range 20 {
		failures = append(failures, New(InvalidInput, strings.Repeat("a", 60)+string(rune('a'+i)), "Correct this field and retry.", errors.New("private")))
	}
	got := Classify(errors.Join(failures...))
	for text, limit := range map[string]int{got.Reason(): 80, got.Remedy(): 120} {
		if err := CheckMessage(text, limit); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, failure := range failures {
		var public *Error
		if !errors.As(failure, &public) || !strings.Contains(wire.Detail, public.Reason()) {
			t.Fatal("public detail lost")
		}
	}
	if strings.Contains(string(raw), "private") {
		t.Fatal("private cause leaked")
	}
	explicit := New(InvalidInput, "Invalid field.", "Correct the named field.", nil)
	explicit.Detail = strings.Repeat("field context ", 100)
	if Classify(explicit).Detail != explicit.Detail {
		t.Fatal("classification truncated explicit detail")
	}
}
