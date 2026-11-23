package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestBridgeFooterEOFStopsRefresh(t *testing.T) {
	bindings := fake.Bindings()
	started := make(chan struct{})
	bindings.RunList.Handler = &fake.Handler[apicontract.RunListArgs, apicontract.RunPage]{Run: func(ctx context.Context) (apicontract.RunPage, error) {
		close(started)
		<-ctx.Done()
		return apicontract.RunPage{}, ctx.Err()
	}}
	p := newBridgePeer(t, bridgeProfile(t, streamingTransport(server(t, bindings))))
	p.initialize()
	footerReply(t, p, "footer")
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	select {
	case <-started:
	case <-deadline.C:
		t.Fatal("footer refresh did not start")
	}
	if err := p.input.Close(); err != nil {
		t.Fatal(err)
	}
	deadline.Reset(time.Second)
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline.C:
		t.Fatal("EOF did not cancel the footer refresh within one second")
	}
}

func footerPage(now time.Time) apicontract.RunPage {
	started, finished := now.Add(-50*time.Minute), now.Add(-5*time.Minute)
	failed := stepmodel.RunSummary{ID: "47c0aa006d128a0325afe873e65cde30", Repo: "github/oberthci/oberth", Ref: "refs/heads/own-v0.30.10",
		Status: stepmodel.RunFailed, CreatedAt: started.Add(-time.Minute), StartedAt: &started, FinishedAt: &finished,
		LogicalStepCounts: stepmodel.LogicalStepCounts{Total: 4, Exact: true, Passed: 2, Failed: 1},
		Failure:           &stepmodel.FailureRecord{Step: "lint", Name: "lint​"}}
	running := stepmodel.RunSummary{ID: "47c0aa00ffffffffffffffffffffffff", Repo: "github/oberthci/oberth", Ref: "refs/tags/v1",
		Status: stepmodel.RunRunning, CreatedAt: now.Add(-42 * time.Second), LogicalStepCounts: stepmodel.LogicalStepCounts{Total: 3}}
	return apicontract.RunPage{Runs: []apicontract.RunRecord{{RunSummary: running}, {RunSummary: failed}}, ServerTime: now}
}

func footerReply(t *testing.T, p *bridgePeer, id string) string {
	t.Helper()
	p.send(id, "resources/read", map[string]any{"uri": footerURI, "_meta": map[string]any{"com.openai/codex-statusline-state": map[string]any{"thread": map[string]any{"id": "t"}}}})
	var reply struct {
		ID     string
		Result struct {
			Contents []struct{ URI, MimeType, Text string }
		}
	}
	if err := json.Unmarshal(p.frame(), &reply); err != nil || reply.ID != id || len(reply.Result.Contents) != 1 {
		t.Fatalf("footer read: %v %+v", err, reply)
	}
	c := reply.Result.Contents[0]
	if c.URI != footerURI || c.MimeType != "application/json" || !json.Valid([]byte(c.Text)) {
		t.Fatalf("footer content: %+v", c)
	}
	return c.Text
}

// Codex picks a statusline provider from a tool's entrypoint and reads its resource about
// once a second; the bridge serves that from a run list refreshed every 15 seconds.
func TestBridgeCodexFooter(t *testing.T) {
	b := fake.Bindings()
	clock := fake.NewClock()
	list := &fake.Handler[apicontract.RunListArgs, apicontract.RunPage]{Value: footerPage(clock.Now())}
	b.RunList.Handler = list
	cfg := bridgeProfile(t, streamingTransport(server(t, b)))
	cfg.Config.Now = clock.Now
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.send("tools", "tools/list", nil)
	var tools struct {
		Result struct {
			Tools []struct {
				Name, Title string
				Meta        map[string]struct{ Entrypoints []map[string]string } `json:"_meta"`
			}
		}
	}
	if err := json.Unmarshal(p.frame(), &tools); err != nil {
		t.Fatal(err)
	}
	advertised := 0
	for _, tool := range tools.Result.Tools {
		if points := tool.Meta["openai/ui"].Entrypoints; len(points) > 0 {
			advertised++
			if tool.Name != "run_list" || tool.Title != "Oberth runs" || len(points) != 1 || points[0]["type"] != "statusline" || points[0]["resourceUri"] != footerURI {
				t.Fatalf("entrypoint on %s: %+v %q", tool.Name, points, tool.Title)
			}
		}
	}
	if advertised != 1 {
		t.Fatalf("statusline entrypoints advertised: %d", advertised)
	}
	text := footerReply(t, p, "r1")
	for _, want := range []string{`"type":"row"`, `"gap":2`, `47c0aa006`, `oberth own-v0.30.10 ✗ failed 3/4 45m00s 51m lint"`, `"color":"red"`,
		`47c0aa00f oberth v1 ▶ running 0/≥3 42s 42s"`, `"color":"yellow"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("footer lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "​") {
		t.Fatalf("footer kept a format character: %s", text)
	}
	footerReply(t, p, "r2")
	inputs, _, calls := list.Observed()
	if calls != 1 || inputs[0].Limit != footerRuns {
		t.Fatalf("a fresh footer read the server again: %d %+v", calls, inputs)
	}
	clock.Advance(footerRefresh)
	footerReply(t, p, "r3")
	deadline := time.Now().Add(2 * time.Second)
	for _, _, calls = list.Observed(); calls != 2 && time.Now().Before(deadline); _, _, calls = list.Observed() {
		time.Sleep(10 * time.Millisecond)
	}
	if calls != 2 {
		t.Fatalf("a stale footer was not refreshed: %d", calls)
	}
}

func TestBridgeCodexFooterStates(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	refused := publicerr.New(publicerr.Forbidden, "Run history is not visible.", "Check this uplink's scope.", nil)
	for name, c := range map[string]struct {
		page apicontract.RunPage
		err  error
		want string
	}{
		"empty":   {apicontract.RunPage{}, nil, `{"children":["Oberth: no runs"],"props":{"color":"status-metadata"},"type":"text"}`},
		"refused": {apicontract.RunPage{}, refused, `{"children":["Oberth: Run history is not visible."],"props":{"color":"red"},"type":"text"}`},
	} {
		if got := string(footerRows(c.page, c.err, now)); got != c.want {
			t.Errorf("%s: %s", name, got)
		}
	}
	for d, want := range map[time.Duration]string{0: "0s", 42 * time.Second: "42s", 190 * time.Second: "3m10s", 2*time.Hour + 5*time.Minute: "2h05m", 28 * time.Hour: "1d04h"} {
		if got := span(d, 2); got != want {
			t.Errorf("span(%s) = %s, want %s", d, got, want)
		}
	}
	if got := span(28*time.Hour, 1); got != "1d" {
		t.Errorf("one-unit span: %s", got)
	}
}

// Other resources stay the server's: a run resource read reaches it unchanged.
func TestBridgeFooterLeavesRunResources(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunPassed}}}
	p := newBridgePeer(t, bridgeProfile(t, streamingTransport(server(t, b))))
	p.initialize()
	p.send("run", "resources/read", map[string]any{"uri": "run://run1"})
	if data := p.frame(); !strings.Contains(string(data), `run://run1`) || strings.Contains(string(data), "Oberth:") {
		t.Fatalf("run resource: %s", data)
	}
}
