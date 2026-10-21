package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/service"
)

func TestAuditCLIForwardsFiltersToTheMCPTool(t *testing.T) {
	const token = "test-audit-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	err := runAudit(context.Background(), []string{"--json", "--action", "secret_access.", "--actor", "agent@host", "--repo", "oberth", "--since", "24h", "--limit", "5", "--before", "40"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	want := `tool=audit_list args={"action":"secret_access.","actor":"agent@host","before":40,"limit":5,"repo":"oberth","since":"24h"}`
	if !strings.Contains(output.String(), want) {
		t.Fatalf("CLI request = %s, want %s", output.String(), want)
	}
	for _, bad := range [][]string{{"--tail", "--before", "3"}, {"--bogus"}, {"stray"}} {
		if err := runAudit(context.Background(), bad, &output); err == nil || (bad[0] != "stray" && !errors.Is(err, errUsage)) {
			t.Fatalf("audit %v = %v, want usage error", bad, err)
		}
	}
}

type auditCallFunc func(context.Context, string, any) (json.RawMessage, error)

func (call auditCallFunc) call(ctx context.Context, tool string, args any) (json.RawMessage, error) {
	return call(ctx, tool, args)
}

func TestAuditTailDoesNotRepeatAppendBetweenHeadAndPage(t *testing.T) {
	previous := auditTailInterval
	auditTailInterval = time.Millisecond
	t.Cleanup(func() { auditTailInterval = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	calls := 0
	remote := auditCallFunc(func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		// Row 11 appeared after the initial head read but before its page.
		return json.Marshal(service.AuditListResponse{HeadID: int64(9 + calls), Entries: []service.AuditEntryView{{ID: 11}}})
	})
	var output strings.Builder
	if err := tailAudit(ctx, remote, nil, true, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if calls != 2 || len(lines) != 1 {
		t.Fatalf("calls=%d output=%s", calls, output.String())
	}
}

func TestAuditTailPollDrainsBurstWithoutSkippingConcurrentAppends(t *testing.T) {
	t.Parallel()
	calls := 0
	remote := auditCallFunc(func(_ context.Context, tool string, raw any) (json.RawMessage, error) {
		calls++
		args := raw.(map[string]any)
		if tool != "audit_list" || args["action"] != "backup." || args["limit"] != 200 {
			t.Fatalf("call %s %#v", tool, args)
		}
		top := int64(450)
		if before, ok := args["before"].(int64); ok {
			top = before - 1
		}
		wantTop := int64(450 - 200*(calls-1))
		if top != wantTop {
			t.Fatalf("page %d top=%d want=%d", calls, top, wantTop)
		}
		page := service.AuditListResponse{HeadID: 449}
		if calls > 1 {
			page.HeadID = 451
		} // appended above the first page
		for id := top; id > max(int64(0), top-200); id-- {
			page.Entries = append(page.Entries, service.AuditEntryView{ID: id})
		}
		if len(page.Entries) == 200 {
			page.NextBefore = page.Entries[199].ID
		}
		return json.Marshal(page)
	})
	rows, high, err := pollAudit(t.Context(), remote, map[string]any{"action": "backup."}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(rows) != 449 || high != 450 {
		t.Fatalf("calls=%d rows=%d high=%d", calls, len(rows), high)
	}
	for i, row := range rows {
		if row.ID != int64(450-i) {
			t.Fatalf("row %d = %d", i, row.ID)
		}
	}
	remote = auditCallFunc(func(context.Context, string, any) (json.RawMessage, error) {
		return json.Marshal(service.AuditListResponse{HeadID: 451, Entries: []service.AuditEntryView{{ID: 451}, {ID: 450}}})
	})
	rows, high, err = pollAudit(t.Context(), remote, nil, high)
	if err != nil || high != 451 || len(rows) != 1 || rows[0].ID != 451 {
		t.Fatalf("next poll=%+v high=%d err=%v", rows, high, err)
	}
}

func TestAuditTransportFlagsSorted(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	transport := mcpTransportFlags(flags)
	if err := flags.Parse([]string{"--url", "https://example.test/mcp", "--token-file", "/token", "--ca-file", "/ca"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(transport(), " "); got != "--ca-file=/ca --token-file=/token --url=https://example.test/mcp" {
		t.Fatal(got)
	}
}
