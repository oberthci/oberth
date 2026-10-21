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
	for _, want := range []string{`query.set("before", String(state.runCursor))`, `"/api/runs/counts"`, `data-action="runs-older"`, `state.runCounts.ByRepo?.[repo.ID]`,
		// The older-page cursor is the one the server names (#073): a run ID
		// for a scoped uplink, which sees no sequences.
		`response.headers.get("X-Oberth-Next-Before")`, `{ withNext: true }`} {
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

// The dashboard renders the default flat list once. Queue rows carry their
// place and the head badge in that table; there are no separate sections.
func TestRunsPageIsOneFlatFilteredList(t *testing.T) {
	t.Parallel()
	script, ok := staticAssets["app.js"]
	if !ok {
		t.Fatal("app.js is not embedded")
	}
	source := string(script.body)
	section := func(start, end string) string {
		t.Helper()
		from := strings.Index(source, start)
		if from < 0 {
			t.Fatalf("app.js is missing %s", start)
		}
		to := strings.Index(source[from:], end)
		if to < 0 {
			t.Fatalf("app.js has nothing after %s", start)
		}
		return source[from : from+to]
	}
	for _, want := range []string{"`/api/runs?limit=${runPageSize}`", `new URLSearchParams({ limit: String(runPageSize) })`, `api("/api/runs/counts")`} {
		if !strings.Contains(source, want) {
			t.Fatalf("app.js run loading is missing %s", want)
		}
	}
	render := section("async function renderRuns(seq)", "\n/* ---------- run detail")
	for _, want := range []string{"loadRunCounts()", "setAuto(listPollMs)", "runsTable(visible)", "runFilters.map"} {
		if !strings.Contains(render, want) {
			t.Fatalf("renderRuns is missing %s", want)
		}
	}
	for _, unwanted := range []string{"activeSection(", "queueTable(", "runningTable(", "Queue head blocked by", "section-title", "section-sub"} {
		if strings.Contains(render, unwanted) || strings.Contains(source, "function "+unwanted) {
			t.Fatalf("runs page still has separate section %q", unwanted)
		}
	}
	blocker := section("function queueBlocker(", "\nfunction queuePosition(")
	for _, want := range []string{"admissionOf(run)?.head_of_queue"} {
		if !strings.Contains(blocker, want) {
			t.Fatalf("queueBlocker is missing the gate's head marker %s", want)
		}
	}
	position := section("function queuePosition(", "\nfunction queueSize(")
	if !strings.Contains(position, "String(run.Scheduling.queue_position)") || strings.Contains(position, "`#") || strings.Contains(position, "q#") {
		t.Fatalf("queue position is not one plain 1..N numbering: %s", position)
	}
	table := section("function runsTable(", "\nasync function renderRuns(")
	for _, want := range []string{"queuePosition(run)", "queueSize(run)", `" blocking"`, `>head</span>`, "runMessageCell(run, blocker)", "runTimeLabel(run)"} {
		if !strings.Contains(table, want) {
			t.Fatalf("flat runs table is missing %s", want)
		}
	}
	if !strings.Contains(source, "needs ${admission.weight} weight; ${admission.used}/${admission.capacity} used") {
		t.Fatal("the blocking run does not say what it needs")
	}
	css := string(staticAssets["app.css"].body)
	for _, want := range []string{".trow.blocking", ".queue-pos", ".run-successor"} {
		if !strings.Contains(css, want) {
			t.Fatalf("app.css is missing %s", want)
		}
	}
}

func TestDashboardUsesTotalRangesAndIssueFacets(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	for _, want := range []string{
		"function pageRange(start, length, total)",
		"pageRange(state.runStart, dated.length, datedTotal)",
		"state.runStart += state.runSourceCount",
		"pageRange(issueStart, issues.length, total)",
		"const facets = page.counts || {}",
		"${facets.open || 0} open · ${facets.manual || 0} manual · ${facets.ci || 0} CI · ${facets.closed || 0} closed",
		"kindCount(value)", "stateCount(value)",
		`query.set("query", state.issueQuery)`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("dashboard is missing count or range logic %q", want)
		}
	}
	if strings.Contains(source, "${issues.length} shown") || strings.Contains(source, "${visible.length} shown") {
		t.Fatal("dashboard still uses page-only shown counts")
	}
}

func TestRunsFiltersCountsAndCancelledSuccessors(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	for _, want := range []string{
		`const runFilters = ["all", "queued", "running", "passed", "failed", "cancelled"]`,
		`const statusOK = state.filter === "all" || displayRunStatus(run) === state.filter`,
		`return statusOK && (!state.releaseOnly || run.Release)`,
		`data-kind-filter="release"`, `state.releaseOnly = !state.releaseOnly`,
		`localStorage.setItem("oberth-kind", state.releaseOnly ? "release" : "all")`,
		`if (saved === "waiting") return "queued"`,
		`if (saved === "interrupted" || saved === "superseded") return "cancelled"`,
		`localStorage.getItem("oberth-filter") === "release"`,
		`state.runCounts?.Running`, `state.runCounts?.RunningByRepo?.[selectedRepo?.ID]`,
		`state.runCounts?.Queued`, `state.runCounts?.QueuedByRepo?.[selectedRepo?.ID]`,
		`const datedTotal = Math.max(0, (counts.get(state.repo) || 0) - queued)`,
		`state.runSourceCount = dated.length`,
		`function shortSuccessor(s) { return String(s || "").slice(0, 12); }`,
		`view successor ${esc(shortSuccessor(run.SupersededBy))}`,
		`status === "cancelled" ?`, `esc(runMessage(run))`, `server restart, retry needed`, `retry-warning`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	for _, unwanted := range []string{`view=active`, `loadActiveRuns`, `SUPERSEDED`, `return "delivered"`, `[pending] running`, `[pending] queued`} {
		if strings.Contains(source, unwanted) {
			t.Fatalf("dashboard retains %q", unwanted)
		}
	}
}

func TestArchivedRepositoryFilterKeepsCatalogAndHistoryCounts(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	for _, required := range []string{
		`repoArchive: "active"`,
		`function listedRepos()`,
		`Boolean(repo.ArchivedAt) === (state.repoArchive === "archived")`,
		`const rows = listedRepos().map`,
		`${listedRepos().map(repo =>`,
		`data-repo-archive=`,
		`state.repoArchive = target.dataset.repoArchive; route();`,
		`${state.repos.length} configured`,
		`state.runCounts.ByRepo?.[repo.ID]`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("archive filter missing %q", required)
		}
	}
}
