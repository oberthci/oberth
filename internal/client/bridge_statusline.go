package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

// Codex draws its footer from a statusline entrypoint on one listed tool: it reads the
// entrypoint's resource about once a second and renders the returned UI tree, giving up
// on a read that takes over a second. The bridge answers that read itself from a cached
// run list refreshed in the background at most every footerRefresh, the plugin run box's
// interval, so a footer costs the server one small list per refresh.
const (
	footerURI     = "ui://oberth/runs"
	footerTool    = "run_list"
	footerRuns    = 3
	footerRefresh = 15 * time.Second
)

type footer struct {
	mu      sync.Mutex
	tree    json.RawMessage
	at      time.Time
	pending bool
}

func isFooterRead(req rpcRequest) bool {
	var params struct {
		URI string `json:"uri"`
	}
	return req.Method == "resources/read" && len(req.ID) > 0 && json.Unmarshal(req.Params, &params) == nil && params.URI == footerURI
}

func (b *bridge) footerRead(ctx context.Context, req rpcRequest) {
	content := map[string]string{"uri": footerURI, "mimeType": "application/json", "text": string(b.footerTree())}
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"contents": []any{content}}})
	_ = b.emit(ctx, data)
}

// footerTree returns the cached tree or a placeholder immediately. A host read starts a
// refresh only when the tree is stale; server latency never delays the host reply.
func (b *bridge) footerTree() json.RawMessage {
	f := &b.footer
	f.mu.Lock()
	tree := f.tree
	if !f.pending && (tree == nil || b.client.now().Sub(f.at) >= footerRefresh) {
		f.pending = true
		b.workers.Go(func() { b.refreshFooter(b.eventCtx) })
	}
	f.mu.Unlock()
	if tree != nil {
		return tree
	}
	return footerText("status-metadata", "Oberth: reading runs")
}

func (b *bridge) refreshFooter(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var page apicontract.RunPage
	err := b.client.Invoke(ctx, footerTool, apicontract.RunListArgs{Limit: footerRuns}, &page)
	now := b.client.now()
	tree := footerRows(page, err, now)
	f := &b.footer
	f.mu.Lock()
	f.tree, f.at, f.pending = tree, now, false
	f.mu.Unlock()
}

// footerStatus is a run's icon and status word and the color the run box gives it.
func footerStatus(status string) (string, string) {
	switch status {
	case "queued":
		return "… queued", "yellow"
	case "running":
		return "▶ running", "yellow"
	case "passed":
		return "✓ passed", "green"
	case "failed":
		return "✗ failed", "red"
	case "cancelled":
		return "■ cancelled", "status-metadata"
	}
	return status, "status-metadata"
}

// footerRows shows the run box's cells on one wrapping line: id, repository, ref, status,
// steps, time, age and a failed run's step.
func footerRows(page apicontract.RunPage, err error, now time.Time) json.RawMessage {
	if err != nil {
		return footerText("red", "Oberth: "+clean(publicerr.Classify(err).Reason()))
	}
	if len(page.Runs) == 0 {
		return footerText("status-metadata", "Oberth: no runs")
	}
	if !page.ServerTime.IsZero() {
		now = page.ServerTime
	}
	children := []any{map[string]any{"type": "bold", "children": []any{"Oberth"}}}
	for _, run := range page.Runs {
		text, color := footerRun(run, page.Runs, now)
		children = append(children, map[string]any{"type": "text", "props": map[string]any{"color": color}, "children": []any{text}})
	}
	data, _ := json.Marshal(map[string]any{"type": "row", "props": map[string]any{"gap": 2, "wrap": "wrap"}, "children": children})
	return data
}

func footerRun(run apicontract.RunRecord, runs []apicontract.RunRecord, now time.Time) (string, string) {
	status, color := footerStatus(string(run.Status))
	done := run.Passed + run.Failed + run.Skipped
	steps := fmt.Sprintf("%d/%d", done, run.Total)
	if !run.Exact {
		steps = fmt.Sprintf("%d/≥%d", done, run.Total)
	}
	start, end := run.CreatedAt, now
	if run.StartedAt != nil {
		start = *run.StartedAt
	}
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	cells := []string{footerID(run.ID, runs), run.Repo[strings.LastIndex(run.Repo, "/")+1:],
		strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/"),
		status, steps, span(end.Sub(start), 2), span(now.Sub(run.CreatedAt), 1)}
	if run.Status == "failed" && run.Failure != nil && run.Failure.Name != "" {
		cells = append(cells, run.Failure.Name)
	}
	return clean(strings.Join(cells, " ")), color
}

func footerText(color, text string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"type": "text", "props": map[string]any{"color": color}, "children": []any{text}})
	return data
}

// footerID is the shortest prefix of at least eight characters no other shown run shares.
func footerID(id string, runs []apicontract.RunRecord) string {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return id
	}
	size := 8
	for size < len(id) && footerShared(id, size, runs) {
		size++
	}
	return id[:size]
}

func footerShared(id string, size int, runs []apicontract.RunRecord) bool {
	for _, run := range runs {
		if run.ID != id && strings.HasPrefix(run.ID, id[:size]) {
			return true
		}
	}
	return false
}

// span is 42s, 3m10s, 2h05m, 1d04h with two units; 42s, 3m, 2h, 1d with one.
func span(d time.Duration, units int) string {
	sizes := []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second}
	names := "dhms"
	d = max(0, d).Truncate(time.Second)
	for i, size := range sizes {
		if d < size {
			continue
		}
		head := fmt.Sprintf("%d%c", d/size, names[i])
		if units == 1 || i == len(sizes)-1 {
			return head
		}
		return head + fmt.Sprintf("%02d%c", d%size/sizes[i+1], names[i+1])
	}
	return "0s"
}

// clean drops control and format characters, as the run box does.
func clean(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
}

// withFooter advertises the footer on run_list. Hosts without statusline support ignore
// the entrypoint; a tool title is its label in Codex's provider picker.
func withFooter(tool map[string]json.RawMessage) {
	var name string
	if json.Unmarshal(tool["name"], &name) != nil || name != footerTool {
		return
	}
	tool["title"], _ = json.Marshal("Oberth runs")
	tool["_meta"], _ = json.Marshal(map[string]any{"openai/ui": map[string]any{"entrypoints": []any{map[string]string{"type": "statusline", "resourceUri": footerURI}}}})
}
