package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestInstallationStateSurvivesMCPReleaseBoundary(t *testing.T) {
	for _, version := range []string{"v0.30.18", "v0.30.19"} {
		t.Run(version, func(t *testing.T) {
			b := fake.Bindings()
			b.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Version: version, SchemaVersion: 6}}
			remote := installationRemote(t, b)
			status, err := remote.InstallationState(t.Context())
			if err != nil || status.Version != version || status.SchemaVersion != 6 {
				t.Fatal(status, err)
			}
		})
	}
}

func installationRemote(t *testing.T, b apicontract.Bindings) *Client {
	t.Helper()
	h := server(t, b)
	remote, err := New(Config{Options: Options{URL: "https://oberth.local/mcp", TokenFile: privateFile(t, "access", []byte("valid"))}, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/mcp" {
			t.Fatal("installation depends on matching MCP versions")
		}
		return handlerTransport(h)(r)
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	return remote
}

func TestInstallationReceiptsSurviveMCPReleaseBoundary(t *testing.T) {
	for _, transition := range []struct{ from, to, action string }{
		{"v0.30.18", "v0.30.19", "upgrade.completed"},
		{"v0.30.19", "v0.30.18", "upgrade.completed"},
		{"v0.30.19", "v0.30.18", "rollback.completed"},
		{"v0.30.19", "v0.30.19", "upgrade.completed"},
	} {
		t.Run(transition.from+"/"+transition.to+"/"+transition.action, func(t *testing.T) {
			b := fake.Bindings()
			records := &fake.Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{State: "recorded"}}
			b.AdminAuditRecord.Handler = records
			remote := installationRemote(t, b)
			args := apicontract.AdminAuditRecordArgs{Action: transition.action, Details: map[string]string{"from": transition.from, "to": transition.to, "revision": "2"}}
			receipt, err := remote.RecordInstallation(t.Context(), args)
			if err != nil || receipt.State != "recorded" {
				t.Fatal(receipt, err)
			}
			got, _, calls := records.Observed()
			if calls != 1 || !reflect.DeepEqual(got, []apicontract.AdminAuditRecordArgs{args}) {
				t.Fatal("completion missing or replayed", calls, got)
			}
		})
	}
}

func TestInstallationReceiptLostReplyIsNeverRetried(t *testing.T) {
	calls := 0
	remote, err := New(Config{Options: Options{URL: "https://oberth.local/mcp"}, Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.ErrUnexpectedEOF
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	_, err = remote.RecordInstallation(t.Context(), apicontract.AdminAuditRecordArgs{Action: "upgrade.completed"})
	if err == nil || calls != 1 {
		t.Fatal("lost receipt was confirmed or replayed", calls, err)
	}
}

func TestInstallationReplyRequiresBoundedJSON(t *testing.T) {
	for _, body := range []string{`{`, strings.Repeat("x", apicontract.AdminBytes+1), `{"code":"forbidden","reason":"Administrator access required.","remedy":"Use an administrator profile."}`} {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = w.WriteString(body)
		var result apicontract.Receipt
		if err := installationReply(w.Result(), definition(t, "admin_audit_record"), &result); err == nil {
			t.Fatal("invalid reply accepted")
		}
	}
}
