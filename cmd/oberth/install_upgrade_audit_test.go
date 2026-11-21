//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

type auditedUpgrade struct {
	server          *composedFixture
	install         signedMaintenance
	app             application
	out, diagnostic *bytes.Buffer
	attempts        *int
}

func TestComposedUpgradeRecordsCompletionWithoutReplayingOnAuditFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"recorded", "refused", "lost reply", "upgrade failed", "dry run"} {
		t.Run(mode, func(t *testing.T) { composedUpgradeAuditCase(t, mode) })
	}
}

func composedUpgradeAuditCase(t *testing.T, mode string) {
	t.Helper()
	f := newAuditedUpgrade(t, mode)
	remote := f.server.administratorClient(t)
	assertUpgradeAudit(t, remote, nil)
	args := []string{"upgrade", "--version", "v0.2.0", "--release-dir", f.install.target, "--yes"}
	if mode == "dry run" {
		args = append(args, "--dry-run")
	}
	code := f.app.run(t.Context(), args)
	if mode == "upgrade failed" {
		if code != 1 || *f.attempts != 0 {
			t.Fatal("failed upgrade recorded completion", code, *f.attempts)
		}
		assertUpgradeAudit(t, remote, nil)
		return
	}
	if code != 0 {
		t.Fatal("completed upgrade failed", code, f.diagnostic)
	}
	if mode == "dry run" {
		if *f.attempts != 0 || f.install.charts.History.Latest.Version != "v0.1.0" {
			t.Fatal("dry run changed installation or audit")
		}
		assertUpgradeAudit(t, remote, nil)
		return
	}
	f.assertCompleted(t)
	if *f.attempts != 1 {
		t.Fatal("audit request replayed or omitted", *f.attempts)
	}
	var expected map[string]string
	if mode != "refused" {
		expected = map[string]string{"from": "v0.1.0", "to": "v0.2.0", "revision": strconv.Itoa(f.install.charts.History.Latest.Number)}
	}
	assertUpgradeAudit(t, remote, expected)
	f.assertWarning(t, mode)
}

// The client reads the profile's certificate before every call, as the real
// transport does, so an audit sent with the files an upgrade removed fails.
func TestUpgradeAndRollbackAuditWithTheProfileFilesTheyLeave(t *testing.T) {
	t.Parallel()
	f := newAuditedUpgrade(t, "recorded")
	native := f.server.client(f.server.config.Listen.HTTPS).Transport.(*http.Transport).Clone()
	certificate := f.server.adminCertificate
	native.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &certificate, nil }
	f.install.session.app.config.Transport = native
	app, _, diagnostic := f.install.rollbackApplication(t)
	before := f.install.session.profile.AdminCert
	if code := app.run(t.Context(), []string{"upgrade", "--version", "v0.2.0", "--release-dir", f.install.target, "--yes"}); code != 0 {
		t.Fatal("upgrade failed", code, diagnostic)
	}
	if _, err := os.Stat(before); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("upgrade kept the earlier certificate file", err)
	}
	if strings.Contains(diagnostic.String(), "unconfirmed") {
		t.Fatal("recorded upgrade audit reported unconfirmed", diagnostic)
	}
	remote := f.server.administratorClient(t)
	revision := strconv.Itoa(f.install.charts.History.Latest.Number)
	assertUpgradeAudit(t, remote, map[string]string{"from": "v0.1.0", "to": "v0.2.0", "revision": revision})
	f.install.session.profile = installedProfile(t, app)
	revision = strconv.Itoa(f.install.charts.History.Latest.Number + 1)
	for _, line := range []string{"Rolled back to v0.1.0, revision " + revision + ".\n", "Already rolled back to v0.1.0, revision " + revision + ".\n"} {
		app, out, diagnostic := f.install.rollbackApplication(t)
		if code := app.run(t.Context(), []string{"rollback", "--release-dir", f.install.previous, "--yes"}); code != 0 || out.String() != line || diagnostic.Len() != 0 {
			t.Fatal("rollback did not say what it did", code, out, diagnostic)
		}
		var page apicontract.AuditPage
		if err := remote.Invoke(t.Context(), "audit_list", apicontract.AuditListArgs{Action: "rollback.completed", Limit: 10}, &page); err != nil || len(page.Records) != 1 {
			t.Fatal("rollback audit missing or repeated", page.Records, err)
		}
		want := map[string]string{"from": "v0.2.0", "to": "v0.1.0", "revision": revision}
		var details map[string]string
		if err := json.Unmarshal([]byte(page.Records[0].Details), &details); err != nil || !reflect.DeepEqual(details, want) {
			t.Fatal("rollback audit records another change", page.Records[0].Details, err)
		}
	}
	assertUpgradeAudit(t, remote, map[string]string{"from": "v0.1.0", "to": "v0.2.0"})
}

func newAuditedUpgrade(t *testing.T, mode string) auditedUpgrade {
	t.Helper()
	server := composeFixture(t, composeBinary(t))
	server.config.Protocol = stateport.DefaultProtocol()
	server.freshDataDirectory(t)
	server.prepareAdministrator(t)
	cancel, done := server.run(t)
	t.Cleanup(cancel)
	server.checkStarting(t, done)
	server.waitStarted(t, done)
	install := signedMaintenanceFixture(t)
	if mode == "upgrade failed" {
		install.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("injected failed readiness") }
	}
	attempts := new(int)
	install.session.app.config = client.Config{Now: server.deps.clock.Now, Transport: auditFailureTransport(t, server.client(server.config.Listen.HTTPS, server.adminCertificate).Transport, mode, attempts)}
	app, out, diagnostic := install.rollbackApplication(t)
	return auditedUpgrade{server: server, install: install, app: app, out: out, diagnostic: diagnostic, attempts: attempts}
}

func auditFailureTransport(t *testing.T, transport http.RoundTripper, mode string, attempts *int) http.RoundTripper {
	t.Helper()
	return testTransport(func(request *http.Request) (*http.Response, error) {
		if request.Body == nil {
			return transport.RoundTrip(request)
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(raw))
		var call struct{ Params struct{ Name string } }
		if err = json.Unmarshal(raw, &call); err != nil {
			return nil, err
		}
		if call.Params.Name != "admin_audit_record" {
			return transport.RoundTrip(request)
		}
		*attempts++
		if mode == "refused" {
			return nil, errors.New("private-audit-marker")
		}
		response, err := transport.RoundTrip(request)
		if err == nil && mode == "lost reply" {
			_ = response.Body.Close()
			return nil, errors.New("private-lost-reply")
		}
		return response, err
	})
}

func (f auditedUpgrade) assertCompleted(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.app.base, "installed", "rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.State != "completed" || record.Archive == nil || f.install.charts.History.Latest.Version != "v0.2.0" {
		t.Fatal("upgrade completion or backup missing", record.State)
	}
	if _, err = os.Stat(record.Archive.Path); err != nil {
		t.Fatal(err)
	}
}

func (f auditedUpgrade) assertWarning(t *testing.T, mode string) {
	t.Helper()
	warning := "Upgrade succeeded; audit unconfirmed. Check: OBERTH_PROFILE='installed' oberth audit\n"
	if mode == "recorded" {
		warning = ""
	}
	if f.diagnostic.String() != warning {
		t.Fatal("upgrade outcome or check command incorrect", f.diagnostic)
	}
	if strings.Contains(f.out.String()+f.diagnostic.String(), "private-") {
		t.Fatal("private audit cause exposed")
	}
}

func assertUpgradeAudit(t *testing.T, remote *client.Client, expected map[string]string) {
	t.Helper()
	var page apicontract.AuditPage
	if err := remote.Invoke(t.Context(), "audit_list", apicontract.AuditListArgs{Action: "upgrade.completed", Limit: 10}, &page); err != nil {
		t.Fatal(err)
	}
	if expected == nil {
		if len(page.Records) != 0 {
			t.Fatal("upgrade audit before completion", page.Records)
		}
		return
	}
	if len(page.Records) != 1 {
		t.Fatal("completed upgrade audit missing or duplicated", page.Records)
	}
	var details map[string]string
	if err := json.Unmarshal([]byte(page.Records[0].Details), &details); err != nil {
		t.Fatal(err)
	}
	for key, want := range expected {
		if details[key] != want {
			t.Errorf("audit %s=%q want %q", key, details[key], want)
		}
	}
}

func (f *composedFixture) checkBackupCompletionAudit(t *testing.T) {
	t.Helper()
	rows, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "backup.completed", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatal("backup stream ended before its completion audit", rows, err)
	}
	remote := f.administratorClient(t)
	var page apicontract.AuditPage
	if err := remote.Invoke(t.Context(), "audit_list", apicontract.AuditListArgs{Action: "backup.completed", Limit: 10}, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatal("existing service backup completion audit missing or duplicated", page.Records)
	}
}
