package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func TestProfileNamespaceIsNotAnOperatorChoice(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.base = t.TempDir()
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(app.base, "selected")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(installer.Profile{Context: "test", Namespace: "oberth", ServerURL: "https://ci.example"})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ command, namespace string }{{"install", ""}, {"install", "oberth"}, {"upgrade", ""}} {
		explicit := tc.namespace
		app.install = func(_ context.Context, r installationRequest, _ io.Reader, _ io.Writer) error {
			s, err := app.openInstallation(r)
			if err == nil && s.choices.Namespace != explicit {
				t.Fatal("profile namespace became a choice", s.choices.Namespace, explicit)
			}
			return err
		}
		args := []string{tc.command, "--profile=selected"}
		if explicit != "" {
			args = append(args, "--namespace="+explicit)
		}
		if code := app.run(t.Context(), args); code != 0 {
			t.Fatal(code, diagnostic.String())
		}
	}
}

func TestObservedStatusRowsHaveNoChoiceMarker(t *testing.T) {
	s, err := store.Create(t.Context(), filepath.Join(t.TempDir(), "state.db"), store.Options{Protocol: stateport.DefaultProtocol(), Clock: func() time.Time { return time.Unix(1000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.RecordAdministration(t.Context(), stateport.Action{Actor: "operator"}, model.AuditAction{Actor: "operator", Action: "backup.completed", ResourceType: "installation"}); err != nil {
		t.Fatal(err)
	}
	rows := settingsRows{origins: map[string]string{"store state": "chosen", "last backup": "chosen"}}
	rows.observations(t.Context(), serverAdministration{&serverOwners{c: &composition{store: s}}}, []apicontract.PartStatus{{Name: "secret store", DisplayReason: "secret store sealed"}})
	for _, row := range rows.values {
		if row.Source != "" {
			t.Fatal("observation reported as a choice", row)
		}
	}
}

func TestUnrecognizedOriginsDoNotReachStatus(t *testing.T) {
	rows := settingsRows{origins: map[string]string{"namespace": "PRIVATE-INVALID-ORIGIN"}}
	rows.add("namespace", "oberth")
	if rows.values[0].Source != "" {
		t.Fatal("unrecognized origin exposed", rows.values)
	}
}
