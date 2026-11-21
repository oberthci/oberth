package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apps "k8s.io/api/apps/v1"
)

func TestCompositionR1EveryViewRequiredAtStartup(t *testing.T) {
	for _, view := range []string{"Counts", "Containers", "ContainerLog"} {
		t.Run(view, func(t *testing.T) {
			f := compositionRegressionFixture(t)
			original := f.deps.constructors.ServiceNew
			f.deps.constructors.ServiceNew = func(ctx context.Context, c *composition) (Part, error) {
				part, err := original(ctx, c)
				switch view {
				case "Counts":
					c.views.Counts.Handler = nil
				case "Containers":
					c.views.Containers.Handler = nil
				case "ContainerLog":
					c.views.ContainerLog.Handler = nil
				}
				return part, err
			}
			if err := serve(t.Context(), f.config, f.deps, f.trace); err == nil || !strings.Contains(err.Error(), "service.Views."+view) {
				t.Fatal("missing view did not stop startup", view, err)
			}
		})
	}
}
func TestCompositionR1ForgeProjectionMatchesRenderedMount(t *testing.T) {
	objects := renderedContractObjects(t)
	var deployment apps.Deployment
	if err := json.Unmarshal(objects["Deployment/oberth"], &deployment); err != nil {
		t.Fatal(err)
	}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "forge" {
			if volume.Secret == nil || volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0440 || volume.Secret.SecretName != "oberth-forge-key" {
				t.Fatal("forge mount contract differs", volume.Name)
			}
			return
		}
	}
	t.Fatal("rendered forge projection missing")
}
func TestCompositionR1ShutdownStopsCallersBeforeService(t *testing.T) {
	f := compositionRegressionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := f.start(ctx)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := strings.Join(f.trace.snapshot(), "\n")
	for _, name := range []string{"wiring", "native"} {
		index := strings.Index(text, "close "+name)
		if index < 0 || index > strings.Index(text, "close service") {
			t.Fatal("service closed before its caller", name, text)
		}
	}
}
