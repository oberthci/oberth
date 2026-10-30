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
	// A queued or declared-but-unreached step is its own row now; it must
	// never repaint as passed, and its state word takes the duration column.
	start := strings.Index(source, "function stepRow(row, selectedKey)")
	end := strings.Index(source, "\nfunction defaultStep(")
	if start < 0 || end <= start {
		t.Fatal("app.js is missing the step row renderer")
	}
	row := source[start:end]
	if !strings.Contains(row, `${sk === "pending" ? " pend" : ""}`) {
		t.Fatal("queued steps do not render as pending rows")
	}
	if !strings.Contains(row, `sk === "cancel" ? "skipped" : statusLabel(step.Status)`) {
		t.Fatal("queued rows do not render the queued state")
	}
	if !strings.Contains(row, `sk === "cancel" ? "⊘" : "◌"`) {
		t.Fatal("queued rows can repaint with a finished glyph")
	}
	if !strings.Contains(source, `case "queued": case "waiting": case "pending": return "pending";`) {
		t.Fatal("queued steps are not classed as pending")
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
		`{groupSteps: !activeBurn, carry: view.carry}`,
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

// The dashboard renders one flat list with grey queued/waiting pills and
// plain-language reasons; queue position and head styling stay out of the rows.
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
	for _, want := range []string{"`/api/runs?limit=${runPageSize}${state.collapseRuns ?", `new URLSearchParams({ limit: String(runPageSize) })`, `api("/api/runs/counts")`} {
		if !strings.Contains(source, want) {
			t.Fatalf("app.js run loading is missing %s", want)
		}
	}
	render := section("async function renderRuns(seq)", "\n/* ---------- run detail")
	for _, want := range []string{"loadRunCounts()", "setAuto(listPollMs)", "runsTable(visible)", "runFilters.map", `<h1 class="sr-only">Runs</h1>`} {
		if !strings.Contains(render, want) {
			t.Fatalf("renderRuns is missing %s", want)
		}
	}
	for _, unwanted := range []string{`class="bar"`, `class="stats"`, "running · ", "const running ="} {
		if strings.Contains(render, unwanted) {
			t.Fatalf("runs page retains summary bar content %q", unwanted)
		}
	}
	for _, unwanted := range []string{"activeSection(", "queueTable(", "runningTable(", "Queue head blocked by", "section-title", "section-sub"} {
		if strings.Contains(render, unwanted) || strings.Contains(source, "function "+unwanted) {
			t.Fatalf("runs page still has separate section %q", unwanted)
		}
	}
	table := section("function runRow(", "\nasync function renderRuns(")
	for _, want := range []string{"queuePhase(run)", "queueSize(run)", "queueSizeTitle(run)", `title="${esc(queueDetail(run))}"`, "runMessageCell(run)", "runTimeLabel(run)"} {
		if !strings.Contains(table, want) {
			t.Fatalf("flat runs table is missing %s", want)
		}
	}
	for _, want := range []string{"queueReason(run)", "waiting for capacity (${b.size}", "waiting behind ${b.repo}/${b.ref} (${b.size})", "waiting for group ${a.group}", "waiting for KubeVirt capacity (repository VM grant)"} {
		if !strings.Contains(source, want) {
			t.Fatalf("plain-language queue status is missing %s", want)
		}
	}
	// The status page also labels the audit-chain head; only the runs table
	// must lose its queue-head badge.
	if strings.Contains(table, ">head</span>") {
		t.Fatal("runs table retains its queue-head badge")
	}
	for _, unwanted := range []string{"queue-pos", `" blocking"`, "weight;"} {
		if strings.Contains(source, unwanted) {
			t.Fatalf("queued rows retain %q", unwanted)
		}
	}
	css := string(staticAssets["app.css"].body)
	// The page heading stays in the accessibility tree but takes no space,
	// leaving the normal .screen padding above the filters and runs list.
	if !strings.Contains(css, ".sr-only{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}") {
		t.Fatal("app.css is missing visually hidden page heading styling")
	}
	for _, unwanted := range []string{".trow.blocking", ".queue-pos"} {
		if strings.Contains(css, unwanted) {
			t.Fatalf("app.css retains queue styling %s", unwanted)
		}
	}
	if !strings.Contains(css, ".run-successor") {
		t.Fatal("app.css lost successor link styling")
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
		`const runFilters = ["all", "queued", "running", "passed", "failed", "infra-retried", "superseded", "cancelled"]`,
		`const statusOK = state.filter === "all" || displayRunStatus(run) === state.filter`,
		`return statusOK && (!state.releaseOnly || run.Release)`,
		`data-kind-filter="release"`, `state.releaseOnly = !state.releaseOnly`,
		`localStorage.setItem("oberth-kind", state.releaseOnly ? "release" : "all")`,
		`if (saved === "waiting") return "queued"`,
		`if (saved === "interrupted") return "cancelled"`,
		`localStorage.getItem("oberth-filter") === "release"`,
		`state.runCounts?.Queued`, `state.runCounts?.QueuedByRepo?.[selectedRepo?.ID]`,
		`const datedTotal = Math.max(0, (counts.get(state.repo) || 0) - queued)`,
		`state.runSourceCount = state.collapseRuns ? source.length : dated.length`,
		`function shortSuccessor(s) { return String(s || "").slice(0, 12); }`,
		`view successor ${esc(shortSuccessor(run.SupersededBy))}`,
		`(status === "cancelled" || status === "superseded") ?`, `esc(runMessage(run))`, `server restart, retry needed`, `retry-warning`,
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

func TestArchivedRepositoriesListedLastWithoutToggle(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	for _, required := range []string{
		`function orderedRepos()`,
		`repo-archived-divider`,
		`repo-filter archived`,
		`const rows = orderedRepos().map`,
		`${state.repos.length} configured`,
		`state.runCounts.ByRepo?.[repo.ID]`,
		`pill("archived", "cancel")`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("archived repository listing missing %q", required)
		}
	}
	for _, unwanted := range []string{"repoArchive", "data-repo-archive", "repoArchiveFilters"} {
		if strings.Contains(source, unwanted) {
			t.Fatalf("archive toggle remains: %s", unwanted)
		}
	}
}

// TestRunDetailStepsAreUniformOrderedRows pins the step list of the run page:
// one kind of row per executed step, in execution order, with the retry and
// secret markers as the only decorations, and no run-level credentialed badge
// anywhere in the dashboard.
func TestRunDetailStepsAreUniformOrderedRows(t *testing.T) {
	t.Parallel()
	source := string(staticAssets["app.js"].body)
	css := string(staticAssets["app.css"].body)
	section := func(start, end string) string {
		from := strings.Index(source, start)
		if from < 0 {
			t.Fatalf("app.js is missing %q", start)
		}
		to := strings.Index(source[from:], end)
		if to < 0 {
			t.Fatalf("app.js is missing %q after %q", end, start)
		}
		return source[from : from+to]
	}
	// The run-level badge is gone from the list, the detail header and the
	// step-list heading; the release badge stays where it was.
	for _, unwanted := range []string{"credentialedBadge", "CREDENTIALED"} {
		if strings.Contains(source, unwanted) {
			t.Fatalf("dashboard retains the credentialed badge: %q", unwanted)
		}
	}
	table := section("function runRow(", "\nasync function renderRuns(")
	if !strings.Contains(table, "${releaseBadge(run)}") {
		t.Fatal("runs table lost the release badge")
	}
	detail := section("async function renderRunDetailView(", "\n/* ---------- repositories")
	if !strings.Contains(detail, `${releaseBadge(run)}`) || !strings.Contains(detail, `${run.Release ? " · RELEASE" : ""}`) {
		t.Fatal("run detail lost the release badge")
	}
	// Every step renders through one row renderer, fed by the ordering
	// function; the grouped burn header and its sub-rows are gone.
	if !strings.Contains(detail, `${orderSteps(steps).map(row => stepRow(row, selected ? selectedKey : "")).join("")`) {
		t.Fatal("run detail does not render ordered uniform step rows")
	}
	for _, unwanted := range []string{"burn-h", "groupSteps(steps)", "burn.steps", "burn.name"} {
		if strings.Contains(detail, unwanted) {
			t.Fatalf("run detail retains grouped burn markup %q", unwanted)
		}
	}
	if strings.Contains(css, ".burn-h") {
		t.Fatal("app.css retains burn header styling")
	}
	// Burns stay contiguous in start/plan order, including pending siblings
	// and legacy retries. The dashboard Node test exercises the live order.
	order := section("function orderSteps(steps)", "\n/*")
	for _, want := range []string{"Date.parse(step.StartedAt", "Number.isFinite(parsed)", "burnStart", "burnOrdinal", "a.identity.attempt - b.identity.attempt", "a.step.Ordinal - b.step.Ordinal"} {
		if !strings.Contains(order, want) {
			t.Fatalf("orderSteps is missing %s", want)
		}
	}
	// Naming: burn/step when they differ, the bare name when they are equal,
	// and a legacy task(N) attempt resolved onto its logical step.
	if !strings.Contains(source, "function stepLabel(identity) { return identity.burn === identity.step ? identity.step : `${identity.burn}/${identity.step}`; }") {
		t.Fatal("stepLabel does not name rows burn/step")
	}
	identity := section("function stepIdentity(step, keys)", "\nfunction stepLabel(")
	for _, want := range []string{`\((\d+)\)$`, "keys.has(`${burn.name}/${name.name}`)", "attempt: 0"} {
		if !strings.Contains(identity, want) {
			t.Fatalf("stepIdentity is missing %s", want)
		}
	}
	// One row shape: icon, name, optional markers, duration or state.
	row := section("function stepRow(row, selectedKey)", "\nfunction defaultStep(")
	for _, want := range []string{
		`<span class="nm">${esc(label)}</span>`,
		"${identity.attempt ? `<span class=\"tag\">retry ${esc(identity.attempt)}</span>` : \"\"}",
		`${step.Secrets ? '<span class="tag">secret</span>' : ""}`,
		`<span class="dur">${esc(column)}</span>`,
		`"<1s"`,
		`sk === "cancel" ? "skipped" : statusLabel(step.Status)`,
	} {
		if !strings.Contains(row, want) {
			t.Fatalf("stepRow is missing %s", want)
		}
	}
	if strings.Count(row, "<button class=\"v4row") != 1 {
		t.Fatal("stepRow renders more than one row shape")
	}
	if !strings.Contains(css, ".v4row .tag{") {
		t.Fatal("app.css is missing the step marker styling")
	}
}
