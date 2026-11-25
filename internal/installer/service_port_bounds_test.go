package installer_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestHTTPSServicePortRefusesInvalidAndUndeclaredChoices(t *testing.T) {
	for _, port := range []int{-1, 2222, 65536} {
		f := setup(t)
		f.choices.HTTPSServicePort = port
		_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
		var problem *installer.Problem
		code := "service-port"
		if port == 2222 {
			code = "service-port-ssh"
		}
		if !errors.As(err, &problem) || problem.Code != code {
			t.Fatal("invalid HTTPS Service port reached preview", port, err)
		}
		assertNoInstallWrites(t, f)
	}
	f := setup(t)
	f.release = loadFixture(t, historicalStatusRelease(t))
	f.choices.HTTPSServicePort = 443
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "service-port-chart" {
		t.Fatal("an old chart silently ignored the requested HTTPS Service port", err)
	}
	assertNoInstallWrites(t, f)
}

func TestChartHTTPSServicePortRefusesIllegalPorts(t *testing.T) {
	for _, port := range []int{-1, 0, 2222, 65536} {
		ch, err := loader.Load(chartPath())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = installer.Render(ch, map[string]any{"servicePort": port}, "oberth"); err == nil {
			t.Fatal("invalid HTTPS Service port passed chart validation", port)
		}
	}
}
