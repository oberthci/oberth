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
		Used: 2, Runs: 2,
		Reservations: []model.AdmissionReservation{
			{RunID: "aaaa", State: "retained", Repo: "exampleorg-ebpf", Ref: "main", Since: now.Add(-90 * time.Second)},
			{RunID: "bbbb", State: "waiting", Repo: "cli", Ref: "feature/x"},
		},
		Blocker: &model.AdmissionBlocker{Repo: "cli", Ref: "feature/x", DiskFloor: true, Used: 2},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"2 run(s) admitted; Argo parallelism bounds running Workflows", "queue head: cli/feature/x (waiting for disk space)", "aaaa  retained", "1m30s", "exampleorg-ebpf/main"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output lacks %q:\n%s", want, text)
		}
	}
}

func TestAdmissionUsage(t *testing.T) {
	for _, arguments := range [][]string{{"release"}, {"release", "a", "b"}, {"--force"}, {"bogus"}, {"--nope"}, {"groups"}, {"capacity", "3"}} {
		if err := runAdmission(context.Background(), arguments, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v, want usage error", arguments, err)
		}
	}
}
