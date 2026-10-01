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

// TestStepStatusProgressOrdersTheLifecycle is what lets three records of the
// same step -- a plan entry, a live marker, a durable result -- be merged in
// any order without a later, less-informed source walking the step backwards.
func TestStepStatusProgressOrdersTheLifecycle(t *testing.T) {
	t.Parallel()

	ordered := []StepStatus{StepPending, StepQueued, StepRunning, StepPassed}
	for index := 1; index < len(ordered); index++ {
		if ordered[index].Progress() <= ordered[index-1].Progress() {
			t.Fatalf("%q does not advance beyond %q", ordered[index], ordered[index-1])
		}
	}
	for _, terminal := range []StepStatus{StepPassed, StepFailed, StepSkipped, StepTimedOut} {
		if terminal.Progress() != StepPassed.Progress() {
			t.Fatalf("terminal status %q ranks apart from the other terminal states", terminal)
		}
	}
	if StepStatus("invented").Progress() >= StepPending.Progress() {
		t.Fatal("an unknown status can displace a planned one")
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
	// pending is a real status the API can carry and is never terminal.
	// PutStepResult's guard is exactly `!Status.Terminal()`, so a pending
	// status that classified as terminal would reach a CHECK constraint that
	// does not admit it and turn a visibility record into a run failure.
	for _, status := range []StepStatus{StepPending, StepQueued, StepRunning} {
		if !status.Valid() || status.Terminal() {
			t.Fatalf("active step status %q classification is inconsistent", status)
		}
	}
	for _, status := range []StepStatus{StepPassed, StepFailed, StepSkipped, StepTimedOut} {
		if !status.Valid() || !status.Terminal() {
			t.Fatalf("step status %q classification is inconsistent", status)
		}
	}
	if StepStatus("unknown").Valid() || StepStatus("unknown").Terminal() {
		t.Fatal("unknown step status reported valid")
	}
}

func TestUpstreamOrg(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"ssh://git@github.com/oberthci":  "oberthci",
		"ssh://git@github.com/OberthCI/": "OberthCI",
		"https://codeberg.org/acme":      "acme",
		"/srv/git/acme":                  "acme",
		// A pathless base URL yields a host-shaped org that no secret path
		// segment can match: upstream-scoped declarations then fail closed at
		// authorization with a register-an-org-scoped-base-URL error.
		"ssh://git@github.com": "git@github.com",
		"":                     "",
		"   ":                  "",
	}
	for base, want := range tests {
		if got := (Upstream{BaseURL: base}).Org(); got != want {
			t.Fatalf("Org(%q) = %q, want %q", base, got, want)
		}
	}
}
