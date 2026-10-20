package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRenderAdmissionShowsRetainedReservations(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	err := renderAdmission(&output, &model.AdmissionState{
		Capacity: 20, Used: 20, Runs: 1, MaxRuns: 20,
		Reservations: []model.AdmissionReservation{
			{RunID: "aaaa", State: "retained", Size: "XL", Weight: 20, Repo: "cloudtaser-ebpf", Ref: "main", Since: now.Add(-90 * time.Second)},
			{RunID: "bbbb", State: "waiting", Size: "M", Weight: 2, Repo: "cli", Ref: "feature/x", Group: "tf"},
		},
		Blocker: &model.AdmissionBlocker{Repo: "cli", Ref: "feature/x", Size: "M", Weight: 2, Used: 20, Capacity: 20},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"weight 20/20 used", "queue head: cli/feature/x (M, needs 2 weight; 20/20 used)", "aaaa  retained", "1m30s", "cloudtaser-ebpf/main"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}
}

func TestAdmissionUsage(t *testing.T) {
	for _, arguments := range [][]string{{"release"}, {"release", "a", "b"}, {"--force"}, {"bogus"}, {"--nope"}} {
		if err := runAdmission(context.Background(), arguments, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v, want usage error", arguments, err)
		}
	}
}
