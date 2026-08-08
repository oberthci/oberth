package model

import "testing"

func TestRunStatusClassification(t *testing.T) {
	t.Parallel()

	for _, status := range []RunStatus{RunQueued, RunRunning} {
		if !status.Valid() || !status.Active() || status.Terminal() {
			t.Fatalf("status %q classification is inconsistent", status)
		}
	}
	for _, status := range []RunStatus{RunPassed, RunFailed, RunInterrupted} {
		if !status.Valid() || status.Active() || !status.Terminal() {
			t.Fatalf("status %q classification is inconsistent", status)
		}
	}
	for _, status := range []RunStatus{"superseded", "unknown"} {
		if status.Valid() {
			t.Fatalf("unsupported run status %q reported valid", status)
		}
	}
}

func TestIssueAndRefEnums(t *testing.T) {
	t.Parallel()

	if !RefBranch.Valid() || !RefTag.Valid() || RefKind("other").Valid() {
		t.Fatal("ref kind validation is inconsistent")
	}
	if !IssueManual.Valid() || !IssueCI.Valid() || IssueKind("other").Valid() {
		t.Fatal("issue kind validation is inconsistent")
	}
	if !IssueOpen.Valid() || !IssueClosed.Valid() || IssueState("other").Valid() {
		t.Fatal("issue state validation is inconsistent")
	}
}

func TestPromotionStatusClassification(t *testing.T) {
	t.Parallel()
	if !PromotionPending.Valid() || PromotionPending.Terminal() {
		t.Fatal("pending promotion status classification is inconsistent")
	}
	for _, status := range []PromotionStatus{PromotionPassed, PromotionFailed, PromotionInterrupted} {
		if !status.Valid() || !status.Terminal() {
			t.Fatalf("promotion status %q classification is inconsistent", status)
		}
	}
}

func TestPublicationStatusClassification(t *testing.T) {
	if !PublicationPending.Valid() || PublicationPending.Terminal() {
		t.Fatal("pending publication classification is invalid")
	}
	for _, status := range []PublicationStatus{PublicationDelivered, PublicationFailed} {
		if !status.Valid() || !status.Terminal() {
			t.Fatalf("terminal publication status %q is invalid", status)
		}
	}
	if PublicationStatus("unknown").Valid() || PublicationStatus("unknown").Terminal() {
		t.Fatal("unknown publication status unexpectedly valid")
	}
}

func TestStepStatusClassification(t *testing.T) {
	t.Parallel()
	for _, status := range []StepStatus{StepPassed, StepFailed, StepSkipped, StepTimedOut} {
		if !status.Valid() || !status.Terminal() {
			t.Fatalf("step status %q classification is inconsistent", status)
		}
	}
	if StepStatus("running").Valid() || StepStatus("running").Terminal() {
		t.Fatal("unknown step status reported valid")
	}
}
