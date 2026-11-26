package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestFreshInstallReviewKeepsNamespaceValuesWhole(t *testing.T) {
	engine, _ := offlineInstallEngine(t, t.TempDir())
	release, err := installer.LocalRelease("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Prepare(t.Context(), release, installer.Choices{Context: "chosen", Namespace: strings.Repeat("control", 8), PipelineNamespace: strings.Repeat("jobs", 12)})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var want string
	for _, line := range plan.Review() {
		if strings.HasPrefix(line, "Namespaces: ") {
			want = line + "\n"
		}
	}
	if len(want) <= 80 || !strings.Contains(want, "; Pod Security: ") {
		t.Fatal("fixture needs an overlong namespace review", want)
	}
	for _, args := range [][]string{{"--dry-run"}, {"--yes"}, nil} {
		flags := installationFlags("install")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		review := installationReview{request: installationRequest{name: "install", flags: flags}, plan: plan}
		if _, err := installationApproval(t.Context(), review, strings.NewReader("n\n"), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), want) {
			t.Fatal("namespace or security value split across lines", args, out.String())
		}
	}
}
