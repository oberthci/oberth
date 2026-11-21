package installer_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestDefaultParallelRunLimit(t *testing.T) {
	t.Parallel()
	parallel := installer.Defaults().Parallel
	if parallel == nil || *parallel != 4 {
		t.Fatal("default run cap must be four", parallel)
	}
}

func TestInstallationAcceptsLowestPorts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		choices installer.Choices
	}{
		{"HTTPS URL", installer.Choices{Context: "test", URL: "https://ci.example:1"}},
		{"public SSH", installer.Choices{Context: "test", SSHPort: 1}},
		{"HTTPS node port", installer.Choices{Context: "test", HTTPSNodePort: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := setup(t)
			f.choices = tc.choices
			prepare(t, f)
			assertNoInstallWrites(t, f)
		})
	}
}

func TestPublicEndpointTrailingSlashNormalizesToOrigin(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.URL = "https://ci.example:30443/"
	prepare(t, f)
	server := f.charts.Previews[0].Values["server"].(map[string]any)
	if got := server["public"].(map[string]any)["url"]; got != "https://ci.example:30443" {
		t.Fatal("endpoint is not an origin", got)
	}
}
