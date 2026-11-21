package app_test

import (
	"os"
	"strings"
	"testing"
)

func TestReviewEnabledPushExplainsRefusal(t *testing.T) {
	t.Parallel()
	enabled := os.Getenv("OBERTH_REVIEW_ASSIST_ENABLED") == "1"
	for _, gated := range []bool{false, true} {
		f := newFixture(t)
		w := workflow()
		w.Spec.Templates[0].Container.Command = nil
		f.setDocument(w)
		s := f.new(t)
		r := receive()
		r.Source = f.source
		r.Updates[0].Request.Assist = true
		if gated {
			if _, err := s.Gate(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			out, err := s.Deliver(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			checkReviewNoticeLines(t, out.Lines(), enabled)
		}
	}
}

func checkReviewNoticeLines(t *testing.T, lines []string, enabled bool) {
	t.Helper()
	got := [2]int{}
	for _, line := range lines {
		if strings.HasPrefix(line, "assist not used:") {
			got[0]++
			if line != "assist not used: build: no explicit command" || len(line) > 80 {
				t.Fatal(line)
			}
		}
		if strings.Contains(line, "assist is not available") {
			got[1]++
		}
	}
	want := [2]int{0, 1}
	if enabled {
		want = [2]int{1, 0}
	}
	if got != want {
		t.Fatal(enabled, lines)
	}
}
