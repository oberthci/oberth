package api

import (
	"strings"
	"testing"
)

func TestRunDetailRefreshKeepsResolvedBreadcrumb(t *testing.T) {
	t.Parallel()
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)
	start := strings.Index(source, "async function renderRunDetail(runID, seq, background)")
	view := strings.Index(source, "async function renderRunDetailView(")
	if start < 0 || view <= start {
		t.Fatal("app.js is missing the run-detail renderers")
	}
	refresh := source[start:view]
	if !strings.Contains(refresh, `if (!background) setChrome("runs", "/ run");`) {
		t.Fatal("run-detail chrome is not limited to foreground navigation")
	}
	if strings.Count(refresh, "setChrome(") != 1 {
		t.Fatal("run-detail refresh contains another chrome update")
	}
	routeStart := strings.Index(source, "async function route(background)")
	routeEnd := strings.Index(source, "/* ---------- events ---------- */")
	if routeStart < 0 || routeEnd <= routeStart {
		t.Fatal("app.js is missing the router")
	}
	router := source[routeStart:routeEnd]
	if !strings.Contains(router, "renderRunDetail(decodeURIComponent(match[1]), seq, background)") {
		t.Fatal("run-detail route does not pass the background refresh state")
	}
	if strings.Contains(router, "setChrome(") {
		t.Fatal("router resets chrome during background refresh")
	}
	viewEnd := strings.Index(source[view:], "\nasync function renderRepos(")
	if viewEnd < 0 {
		t.Fatal("app.js is missing the renderer after run detail")
	}
	if !strings.Contains(source[view:view+viewEnd], "setChrome(\"runs\", `/ ${repository}`);") {
		t.Fatal("run-detail view no longer sets the repository breadcrumb")
	}
}

func TestRunDetailKeepsQueuedBurnsPending(t *testing.T) {
	t.Parallel()
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)
	if !strings.Contains(source, `kinds.includes("pending") ? "pending"`) {
		t.Fatal("queued steps can repaint their burn as passed")
	}
	if !strings.Contains(source, `burn.kind === "pending" ? "queued"`) {
		t.Fatal("queued burn headers do not render the queued state")
	}
}

func TestRunsServerSideRepoFilter(t *testing.T) {
	t.Parallel()
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)

	// Fix 1: /runs filtered view uses the server-side repo query parameter.
	if !strings.Contains(source, `/api/runs?limit=100&repo=`) {
		t.Fatal("app.js does not use the server-side repo filter endpoint")
	}

	// Fix 2: historyStrip emits data-run-id on .hist buttons (not bare <i>).
	histStart := strings.Index(source, "function historyStrip(")
	if histStart < 0 {
		t.Fatal("app.js is missing historyStrip")
	}
	histEnd := strings.Index(source[histStart:], "\n}")
	if histEnd < 0 {
		t.Fatal("app.js historyStrip has no closing brace")
	}
	hist := source[histStart : histStart+histEnd]
	if !strings.Contains(hist, `class="hist `) {
		t.Fatal("historyStrip does not emit .hist class on its elements")
	}
	if !strings.Contains(hist, `data-run-id="`) {
		t.Fatal("historyStrip does not emit data-run-id on history blocks")
	}

	// Fix 2 (ordering): data-run-id check must appear before data-repo-runs
	// in the click handler — history buttons sit inside repo rows, so the
	// reversed order would swallow the click.
	runIDPos := strings.Index(source, `event.target.closest("[data-run-id]")`)
	repoRunsPos := strings.Index(source, `event.target.closest("[data-repo-runs]")`)
	if runIDPos < 0 || repoRunsPos < 0 {
		t.Fatal("app.js is missing data-run-id or data-repo-runs click handlers")
	}
	if runIDPos >= repoRunsPos {
		t.Fatal("data-run-id click handler must appear before data-repo-runs — history buttons nest inside repo rows")
	}
}
