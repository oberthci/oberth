package setuptui

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A saved "install complete" whose release is gone (the cluster was
// reinstalled) reopens Review and Install, keeping the answers (#069).
func TestForgetVanishedInstall(t *testing.T) {
	saved := func() *WizardState {
		st := newWizardState(Options{})
		st.Install = installRecord{Status: "complete", At: time.Date(2026, 10, 18, 0, 23, 0, 0, time.UTC)}
		st.done[stepReview], st.done[stepInstall] = true, true
		st.Config.ContextName = "default"
		return st
	}
	defer func(previous func(context.Context, string, string, string) (bool, bool)) { releasePresent = previous }(releasePresent)

	releasePresent = func(_ context.Context, _, kubeContext, namespace string) (bool, bool) {
		if kubeContext != "default" || namespace != "oberth" {
			t.Errorf("asked about %s/%s", kubeContext, namespace)
		}
		return false, true
	}
	st := saved()
	note := forgetVanishedInstall(context.Background(), st)
	if !strings.Contains(note, "no longer on this cluster") || st.Install.Status != installNone || st.done[stepInstall] || st.done[stepReview] {
		t.Fatalf("vanished install kept: note %q install %+v", note, st.Install)
	}
	if st.Config.ContextName != "default" {
		t.Fatal("answers were dropped")
	}

	for _, answer := range [][2]bool{{true, true}, {false, false}} {
		releasePresent = func(context.Context, string, string, string) (bool, bool) { return answer[0], answer[1] }
		st := saved()
		if note := forgetVanishedInstall(context.Background(), st); note != "" || st.Install.Status != "complete" {
			t.Fatalf("present=%v known=%v: note %q install %+v", answer[0], answer[1], note, st.Install)
		}
	}
}

// The install screen's status line reads table rows, not table bytes (#070).
func TestActivityLineReadsTableRows(t *testing.T) {
	for lines, want := range map[string]string{
		"Waiting for Oberth to become ready...":                                   "Waiting for Oberth to become ready...",
		"│ Upstream identity │ ~/.ssh/id_ed25519 │ ✓ in OpenBao │":                "Upstream identity: ~/.ssh/id_ed25519 · ✓ in OpenBao",
		"│ Upstream identity │ ~/.ssh/id_ed25519 │ ✓ in OpenBao │\n└──────┴────┘": "Upstream identity: ~/.ssh/id_ed25519 · ✓ in OpenBao",
		"\n\n": "",
	} {
		if got := activityLine(strings.Split(lines, "\n")); got != want {
			t.Errorf("activityLine(%q) = %q, want %q", lines, got, want)
		}
	}
}
