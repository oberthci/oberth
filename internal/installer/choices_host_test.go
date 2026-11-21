package installer_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallReviewRefusesUnicodeHostWithPunycodeRecovery(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://bücher.example", "https://xn--bcher-kva.example"},
		{"https://例え.テスト:34443/", "https://xn--r8jz45g.xn--zckzah:34443/"},
		{"https://\u212Aube.local", "https://kube.local"},
		{"https://b%C3%BCcher.example", "https://xn--bcher-kva.example"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			f := setup(t)
			f.choices.URL = pair[0]
			plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			var refusal *installer.Problem
			if plan != nil || !errors.As(err, &refusal) || refusal.Code != "url-host" || refusal.Detail != "--url "+pair[1] || !strings.Contains(refusal.Remedy, "punycode") {
				t.Fatal("Unicode host lacks review-time recovery", err)
			}
			if len(f.charts.Previews) != 0 || len(f.charts.Applies) != 0 {
				t.Fatal("invalid host reached chart review or deployment")
			}
			entries, err := os.ReadDir(f.dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("refused review wrote credentials", err)
			}
			f.choices.URL = strings.TrimPrefix(refusal.Detail, "--url ")
			plan, err = f.engine.Prepare(t.Context(), f.release, f.choices)
			if err != nil {
				t.Fatal("suggested address was refused", err)
			}
			plan.Close()
		})
	}
}

func TestInstallReviewAcceptsASCIIHosts(t *testing.T) {
	for _, address := range []string{"https://ci.example", "https://xn--bcher-kva.example", "https://127.0.0.1:30443", "https://[::1]:30443/"} {
		t.Run(address, func(t *testing.T) {
			f := setup(t)
			f.choices.URL = address
			plan := prepare(t, f)
			plan.Close()
		})
	}
}
