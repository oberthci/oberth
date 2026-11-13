package model

import "testing"

func TestArtifactCollectionNoteSeparatesOnlyFixedSuffix(t *testing.T) {
	for _, base := range []string{"", "Evicted", "Workflow deadline exceeded", "failed: artifact collection deadline exceeded mid-operation"} {
		message := WithArtifactCollectionDeadline(base)
		failure, present := SplitArtifactCollectionDeadline(message)
		if !present || failure != base || WithArtifactCollectionDeadline(message) != message {
			t.Fatal(base, message, failure, present)
		}
	}
	if !IsExecutionDeadlineReason("Workflow deadline exceeded\n" + ArtifactCollectionDeadlineReason) {
		t.Fatal("collection note obscured deadline")
	}
	if _, present := SplitArtifactCollectionDeadline("artifact collection deadline exceeded during a command"); present {
		t.Fatal("arbitrary error text stripped")
	}
}
