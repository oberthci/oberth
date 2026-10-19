package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestRunCancelCLIForwardsToTheMCPTool(t *testing.T) {
	const token = "test-cancel-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	if err := runRunDetail(context.Background(), []string{"cancel", "0123456789abcdef0123456789abcdef", "--reason", "wrong branch", "--force"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=run_cancel args={"force":true,"id":"0123456789abcdef0123456789abcdef","reason":"wrong branch"}`) {
		t.Fatalf("CLI request = %s", output.String())
	}
	output.Reset()
	if err := runRunDetail(context.Background(), []string{"cancel", "--json", "--reason=x", "0123456789ab"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=run_cancel args={"id":"0123456789ab","reason":"x"}`) {
		t.Fatalf("--json CLI request = %s", output.String())
	}
	for _, bad := range [][]string{
		{"cancel"},
		{"cancel", "a", "b"},
		{"cancel", "0123456789ab", "--bogus"},
		{"cancel", "0123456789ab", "--reason", "two\nlines"},
		{"cancel", "0123456789ab", "--database", "/data/oberth.sqlite", "--url", server.URL},
	} {
		if err := runRunDetail(context.Background(), bad, &output); !errors.Is(err, errUsage) {
			t.Fatalf("run %v = %v, want usage error", bad, err)
		}
	}
}

// runs cancel lists the ref through run_list and cancels only its active
// runs, each under the same rules as run cancel; one refusal does not stop
// the others and is returned.
func TestRunsCancelCLICancelsOnlyTheRefsActiveRuns(t *testing.T) {
	const token = "test-runs-cancel"
	var mu sync.Mutex
	var cancelled []string
	var listed map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			testWriteJSONRPCError(w, nil, -32700, "parse error")
			return
		}
		raw, _ := json.Marshal(request.Params)
		var params toolsCallParams
		_ = json.Unmarshal(raw, &params)
		var arguments map[string]any
		_ = json.Unmarshal(params.Arguments, &arguments)
		id := request.ID
		reply := func(value any, isError bool) {
			text, _ := json.Marshal(value)
			testWriteJSONRPCResult(w, &id, toolsCallResult{Content: []mcpContent{{Type: "text", Text: string(text)}}, IsError: isError})
		}
		switch params.Name {
		case "run_list":
			mu.Lock()
			listed = arguments
			mu.Unlock()
			reply(map[string]any{"runs": []map[string]any{
				{"ID": "aaaa", "Status": "queued"}, {"ID": "bbbb", "Status": "waiting"},
				{"ID": "cccc", "Status": "running"}, {"ID": "dddd", "Status": "passed"},
			}}, false)
		case "run_cancel":
			runID, _ := arguments["id"].(string)
			mu.Lock()
			cancelled = append(cancelled, runID)
			mu.Unlock()
			if runID == "cccc" {
				reply(map[string]string{"error": "forbidden"}, true)
				return
			}
			reply(api.RunCancelResponse{RunID: runID, Status: "interrupted", Phase: "cancelled", PreviousState: "queued",
				CancelledBy: "agent@host", Reason: "cancelled by agent@host: " + fmt.Sprint(arguments["reason"]), Workflow: "none"}, false)
		default:
			testWriteJSONRPCError(w, &id, -32601, "method not found")
		}
	}))
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	var output strings.Builder
	err := runRuns(context.Background(), []string{"cancel", "--repo", "beacon", "--ref", "feature-x", "--reason", "abandoned"}, &output)
	if !errors.Is(err, errMCPToolError) || !strings.Contains(err.Error(), "run cccc") {
		t.Fatalf("runs cancel error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if listed["repo"] != "beacon" || listed["ref"] != "feature-x" {
		t.Fatalf("run_list arguments = %v", listed)
	}
	if strings.Join(cancelled, ",") != "aaaa,bbbb,cccc" {
		t.Fatalf("cancelled %v; want only the active runs", cancelled)
	}
	for _, want := range []string{"aaaa  cancelled (was queued; workflow none)  cancelled by agent@host: abandoned", "bbbb  cancelled", "cccc  not cancelled"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q lacks %q", output.String(), want)
		}
	}
	if err := runRuns(context.Background(), []string{"cancel", "--repo", "beacon"}, &output); !errors.Is(err, errUsage) {
		t.Fatalf("runs cancel without --ref = %v", err)
	}
}

// In-pod, the cancel is recorded as the administrative actor with the same
// audit and obligation, and the live daemon is woken over its admin socket to
// run the cancellation pass.
func TestRunCancelInPodRecordsTheCancelAndWakesTheDaemon(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://github.com/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(ref string, kind model.RefKind, trigger, digit string, release bool) model.Run {
		enqueued, err := database.EnqueueRun(ctx, model.RunSpec{RepoID: repository.ID, RefKind: kind, Ref: ref,
			SHA: strings.Repeat(digit, 40), Actor: "pusher@host", Trigger: trigger, Release: release})
		if err != nil {
			t.Fatal(err)
		}
		return enqueued.Run
	}
	claimed := enqueue("feature-pod", model.RefBranch, "branch", "a", false)
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunJobName(ctx, claimed.ID, "oberth-oberth-"+claimed.ID[:8]); err != nil {
		t.Fatal(err)
	}
	release := enqueue("v1.2.3", model.RefTag, "tag", "b", true)

	socket := filepath.Join("/tmp", "oberth-admin-"+filepath.Base(t.TempDir())+".sock")
	woken := make(chan struct{}, 4)
	server := newAdminGateServer(nil, path, func() { woken <- struct{}{} })
	listener, err := listenAdminSocket(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = removeStaleAdminSocket(socket)
	})
	go func() { _ = server.Serve(listener) }()

	pod := &podRunCanceller{database: database, path: path, socket: socket}
	defer pod.close()
	ids, err := pod.activeRuns(ctx, "oberth", "feature-pod")
	if err != nil || len(ids) != 1 || ids[0] != claimed.ID {
		t.Fatalf("in-pod active runs = %v, %v", ids, err)
	}
	response, raw, err := pod.cancel(ctx, claimed.ID[:12], "operator stop", false)
	if err != nil {
		t.Fatal(err)
	}
	if response.RunID != claimed.ID || response.PreviousState != "waiting" || response.Workflow != "pending" ||
		response.CancelledBy != administrativeActor || response.Warning != "" || !json.Valid(raw) {
		t.Fatalf("in-pod cancel = %#v (%s)", response, raw)
	}
	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon was not asked to run its cancellation pass")
	}
	pending, err := database.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 1 || pending[0].RunID != claimed.ID || pending[0].JobName == "" {
		t.Fatalf("in-pod cancel obligation = %#v, %v", pending, err)
	}
	audit, err := database.LatestAuditActionByType(ctx, "run.cancel", "run")
	if err != nil || audit.Actor != administrativeActor || audit.ResourceID != claimed.ID {
		t.Fatalf("in-pod cancel audit = %#v, %v", audit, err)
	}
	if _, _, err := pod.cancel(ctx, claimed.ID, "", false); !errors.Is(err, store.ErrRunNotActive) {
		t.Fatalf("second in-pod cancel = %v", err)
	}
	if _, _, err := pod.cancel(ctx, release.ID, "", false); err == nil || !strings.Contains(err.Error(), "force") {
		t.Fatalf("in-pod release cancel without force = %v", err)
	}
	forced, _, err := pod.cancel(ctx, release.ID, "bad tag", true)
	if err != nil || forced.Workflow != "none" || !strings.Contains(forced.Warning, "partial") {
		t.Fatalf("forced in-pod release cancel = %#v, %v", forced, err)
	}
	var text bytes.Buffer
	if err := writeRunCancel(&text, forced, nil, false); err != nil || !strings.Contains(text.String(), "warning: publication may be partial") {
		t.Fatalf("human output = %q, %v", text.String(), err)
	}
	if err := requestAdminSchedulerWake(ctx, socket, "/tmp/other.sqlite"); err == nil || !strings.Contains(err.Error(), "not the live daemon database") {
		t.Fatalf("wake for another database = %v", err)
	}
}

func TestDisplayRunStatusShowsCancelled(t *testing.T) {
	if got := displayRunStatus(remoteRun{Status: "interrupted", Phase: "cancelled"}); got != "cancelled" {
		t.Fatalf("cancelled run displays %q", got)
	}
	if got := displayRunStatus(remoteRun{Status: "interrupted", Phase: "interrupted"}); got != "interrupted" {
		t.Fatalf("interrupted run displays %q", got)
	}
}
