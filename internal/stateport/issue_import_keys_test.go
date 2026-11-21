package stateport

import "testing"

func TestEveryIssueImportRefusalKeyHasAMessage(t *testing.T) {
	messages := importMessages()
	for key := noImportProblem + 1; key < importProblemCount; key++ {
		pair := messages[key]
		if pair[0] == "" || pair[1] == "" {
			t.Fatal("import refusal key has no complete message", key)
		}
	}
}
