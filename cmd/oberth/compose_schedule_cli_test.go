//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestComposedScheduleRefusedFieldReachesCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, fields, path string }{
		{"pod-metadata", "  podMetadata:\n    annotations:\n      requested-name: 'probe-value{{workflow.name}}'\n", "spec.podMetadata.annotations.requested-name"},
		{"argument", "  arguments:\n    parameters: [{name: x, value: 'probe-value{{workflow.name}}'}]\n", "spec.arguments.parameters[0].value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := composeFixture(t, composeBinary(t))
			f.prepareScheduledObservation(t)
			document := strings.Replace(composedWorkflow, "spec:\n", "spec:\n"+tc.fields, 1)
			f.git.Write(t, ".oberth/build.yaml", []byte(document))
			f.git.CommitFile(t, "refused schedule expression")
			f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
			f.prepareAdministrator(t)
			cancel, done := f.run(t)
			defer cancel()
			f.checkStarting(t, done)
			f.waitStarted(t, done)
			f.assertScheduleRefusalCLI(t, tc.path)
			runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 10})
			if err != nil || len(runs) != 0 {
				t.Fatal("refused schedule recorded a run", err)
			}
		})
	}
}

func (f *composedFixture) assertScheduleRefusalCLI(t *testing.T, path string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	app := application{out: &out, diagnostic: &diagnostic, env: func(key string) string {
		if key == "OBERTH_URL" {
			return f.config.Public.URL
		}
		return ""
	}, config: client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport}}
	if code := app.run(t.Context(), []string{"schedule", "list", "--detail"}); code != 0 {
		t.Fatal("schedule command failed", code, diagnostic.String())
	}
	if !strings.Contains(out.String(), path) || !strings.Contains(out.String(), gittest.Repository) || strings.Contains(out.String(), "probe-value") {
		t.Fatal("CLI changed or lost the producer's refused field", out.String())
	}
}
