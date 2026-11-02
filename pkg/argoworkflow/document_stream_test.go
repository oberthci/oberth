package argoworkflow

import "testing"

func TestDecodeRejectsInlineSecondDocuments(t *testing.T) {
	for _, suffix := range []string{"\n--- {}\n", "\n--- # second\n{}\n", "\n...\n--- {}\n"} {
		if _, err := Decode([]byte(admissibleDocument + suffix)); err == nil {
			t.Fatalf("accepted second document %q", suffix)
		}
	}
}
