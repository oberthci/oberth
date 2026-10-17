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
	if !strings.Contains(source, `query.set("repo", state.repo)`) {
		t.Fatal("app.js does not use the server-side repo filter endpoint")
	}
	// The run list pages with ?before and the sidebar counts every retained
	// run, not just the loaded page (#050).
	for _, want := range []string{`query.set("before", String(state.runCursor))`, `"/api/runs/counts"`, `data-action="runs-older"`, `state.runCounts.ByRepo?.[repo.ID]`} {
		if !strings.Contains(source, want) {
			t.Fatalf("app.js run paging is missing %s", want)
		}
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

// A repository name shared by several upstreams ("terraform" under github,
// codeberg and skipops) is an ambiguous selector the API refuses with 409, so
// the dashboard filters runs and issues by upstream/org/name and labels a
// shared name org/name.
func TestDashboardQualifiesSharedRepositoryNames(t *testing.T) {
	t.Parallel()
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)
	for _, want := range []string{
		"function repoKey(repo)",
		"`${upstream.name}/${repoOrg(upstream)}/${repo.Name}`",
		"`${repoOrg(upstream)}/${repo.Name}`",
		"if (!state.status) return value;",
		`[400, 404, 409].includes(err.status)`,
		`return { pending: ensureStatus() };`,
		`loadStatus({ detached: true })`,
		`(!opts.detached && state.routeSeq !== seq)`,
		`route(true); });`,
		`data-repo-filter="${esc(repoKey(repo))}"`,
		`data-repo-runs="${esc(repoKey(repo))}"`,
		`<option value="${esc(repoKey(repo))}"`,
		`counts.set(repoKey(repo), state.runCounts.ByRepo?.[repo.ID] || 0)`,
		`state.repo = normalizeRepoSelector(state.repo, "all")`,
		`err.status === 409`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("app.js is missing %s", want)
		}
	}
	for _, bare := range []string{`data-repo-filter="${esc(repo.Name)}"`, `data-repo-runs="${esc(repo.Name)}"`, `<option value="${esc(repo.Name)}"`} {
		if strings.Contains(source, bare) {
			t.Errorf("app.js still selects a repository by its bare name: %s", bare)
		}
	}
}

// Argo executor lines are folded behind a toggle, and the combined live view
// prints one header per step instead of the [burn/step] prefix on every line.
func TestDashboardFoldsExecutorLines(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	for _, want := range []string{
		`data-action="toggle-exec"`,
		`case "toggle-exec": toggleExecutorLines(); break;`,
		`class="log-line exec"`,
		`class="log-line log-hdr"`,
		`{ groupSteps: !scoped }`,
		`state.showExec ? " show-exec" : ""`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("app.js is missing %s", want)
		}
	}
	css := string(staticAssets["app.css"].body)
	if !strings.Contains(css, ".log-line.exec{display:none}") || !strings.Contains(css, ".term-body.show-exec .log-line.exec{display:grid}") {
		t.Error("app.css does not hide executor lines unless shown")
	}
}
