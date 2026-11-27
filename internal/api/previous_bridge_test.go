package api_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
	"k8s.io/utils/clock"
)

// The release walk supplies its already verified installed executable.
func TestPreviousReleaseBridgeDisplaysProtocolFloor(t *testing.T) {
	binary := os.Getenv("OBERTH_PREVIOUS_CLIENT")
	if binary == "" {
		t.Skip("set OBERTH_PREVIOUS_CLIENT to the verified v0.30.18 executable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "oberth v0.30.18 ") {
		t.Fatal("previous release executable required", err)
	}
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		// Real socket deadlines require the wall clock, not the recorder's clock.
		c.Runtime.Now = clock.RealClock{}.Now
	})
	server := httptest.NewTLSServer(f.server.Handler())
	defer server.Close()
	base := t.TempDir()
	dir := filepath.Join(base, "oberth", "previous")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(map[string]any{"server_url": server.URL, "uplink": true})
	for name, raw := range map[string][]byte{
		"installation.json": profile,
		"token":             []byte("valid"),
		"ca.crt":            pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(ctx, binary, "mcp", "--profile", "previous")
	command.Env = []string{"HOME=" + base, "XDG_CONFIG_HOME=" + base}
	command.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"walk18","version":"v0.30.18"}}}` + "\n")
	output, err := command.Output()
	if err != nil {
		t.Fatal("previous bridge failed", err)
	}
	var reply struct {
		Error struct {
			Message string
			Data    struct{ Reason, Remedy string }
		}
	}
	if err := json.Unmarshal(output, &reply); err != nil {
		t.Fatal("invalid bridge output", err)
	}
	if reply.Error.Message != "MCP 2026-07-28 is required." || !strings.Contains(reply.Error.Data.Remedy, "client matching the server release") {
		t.Fatal("installed bridge lost floor or remedy", string(output))
	}
}
