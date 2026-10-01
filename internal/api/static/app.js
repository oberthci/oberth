"use strict";
/* Oberth dashboard — resurrected pre-rewrite SPA, adapted for the FAB
   architecture. Talks only to the authenticated read-only views:
   /api/runs, /api/runs/{id}, /api/runs/{id}/logs, /api/repos, /api/issues,
   /api/status. The bearer token persists in localStorage so navigation and
   reloads never re-prompt. No Argo, no reconciliation, no shadow-main. */

const app = document.getElementById("app");
const listPollMs = 4000;
const slowPollMs = 10000;
const livePollMs = 2000;
const state = {
  repos: [], runs: [], repoRuns: [], status: null,
  timer: null, routeSeq: 0,
  filter: localStorage.getItem("oberth-filter") || "all",
  repo: localStorage.getItem("oberth-repo") || "all",
  step: "", stepRunID: "",
  logCache: new Map(),
  live: null, // assigned per-run by liveLogContent
  issueKind: "", issueState: "open", issueRepo: "", issueCursor: 0, issueCursorHistory: [],
  issuePage: null, issueOpener: null,
  lastList: "/runs",
  authGen: 0, // incremented on sign-out and token replacement
  pendingController: null, // AbortController for in-flight API requests
};
const runFilters = ["all", "running", "queued", "passed", "failed", "interrupted", "release"];

/* ---------- tiny utilities (unchanged from the old dashboard) ---------- */
function esc(v) { return String(v ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
function enc(v) { return encodeURIComponent(String(v ?? "")); }
function replaceApp(html) { app.innerHTML = html; }
function shortSha(s) { return s ? String(s).slice(0, 7) : "--"; }
function shortId(s) { s = String(s || ""); return s.length > 13 ? s.slice(0, 13) + "…" : (s || "--"); }
function ago(s) {
  if (!s) return "--";
  const d = new Date(s);
  if (Number.isNaN(+d)) return "--";
  const sec = Math.max(0, Math.round((Date.now() - d) / 1000));
  if (sec < 60) return sec + "s ago";
  const min = Math.round(sec / 60);
  if (min < 60) return min + "m ago";
  const hr = Math.round(min / 60);
  if (hr < 48) return hr + "h ago";
  return Math.round(hr / 24) + "d ago";
}
function fmtTime(s) { if (!s) return "--"; const d = new Date(s); return Number.isNaN(+d) ? "--" : d.toLocaleString(); }
function fmtDur(sec) {
  sec = Math.max(0, Math.round(Number(sec) || 0));
  if (!sec) return "--";
  if (sec < 60) return sec + "s";
  const min = Math.floor(sec / 60), s = sec % 60;
  if (min < 60) return `${min}m ${s}s`;
  return `${Math.floor(min / 60)}h ${min % 60}m`;
}
function spanSeconds(started, finished, active) {
  const start = Date.parse(started || "");
  if (!Number.isFinite(start)) return 0;
  let end = Date.parse(finished || "");
  if (!Number.isFinite(end)) { if (!active) return 0; end = Date.now(); }
  return Math.max(0, (end - start) / 1000);
}

/* ---------- minimal Markdown renderer (issue bodies only) ----------
   Escape-first by construction: every text segment passes esc() BEFORE any
   transform runs, so raw HTML in the source arrives pre-neutralized and can
   never reach the DOM as markup. The transforms emit only h1-h4, p, br, pre,
   code, ul, ol, li, strong, em, and a with an http(s)-allowlisted href plus
   rel="noopener noreferrer" target="_blank". No other tag, no other
   attribute, and no event handler is ever emitted. */
function mdSafeHref(url) { return /^https?:\/\//i.test(url); }
function mdInline(escaped) {
  return escaped.split(/(`[^`]*`)/).map(part => {
    if (part.length > 1 && part.startsWith("`") && part.endsWith("`")) return `<code>${part.slice(1, -1)}</code>`;
    let out = part.replace(/\[([^\]\n]+)\]\(([^()\s]+)\)/g, (all, label, url) =>
      mdSafeHref(url) ? `<a href="${url}" target="_blank" rel="noopener noreferrer">${label}</a>` : all);
    out = out.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    out = out.replace(/(^|[^*])\*([^*]+)\*(?!\*)/g, "$1<em>$2</em>");
    return out;
  }).join("");
}
function renderMarkdown(raw) {
  const lines = String(raw ?? "").replaceAll("\r\n", "\n").replaceAll("\r", "\n").split("\n");
  const out = [];
  let paragraph = [], list = "";
  const closeList = () => { if (list) { out.push(`</${list}>`); list = ""; } };
  const flush = () => { if (paragraph.length) { out.push(`<p>${paragraph.map(mdInline).join("<br>")}</p>`); paragraph = []; } };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^```/.test(line)) {
      flush(); closeList();
      const block = [];
      for (i++; i < lines.length && !/^```/.test(lines[i]); i++) block.push(lines[i]);
      out.push(`<pre class="md-code">${esc(block.join("\n"))}</pre>`);
      continue;
    }
    if (!line.trim()) { flush(); closeList(); continue; }
    const header = line.match(/^(#{1,4})\s+(.+)$/);
    if (header) { flush(); closeList(); const n = header[1].length; out.push(`<h${n}>${mdInline(esc(header[2]))}</h${n}>`); continue; }
    const bullet = line.match(/^\s{0,3}[-*]\s+(.+)$/);
    const ordered = bullet ? null : line.match(/^\s{0,3}\d{1,3}[.)]\s+(.+)$/);
    if (bullet || ordered) {
      flush();
      const kind = bullet ? "ul" : "ol";
      if (list !== kind) { closeList(); out.push(`<${kind}>`); list = kind; }
      out.push(`<li>${mdInline(esc((bullet || ordered)[1]))}</li>`);
      continue;
    }
    closeList();
    paragraph.push(esc(line));
  }
  flush(); closeList();
  return out.join("");
}

/* ---------- status vocabulary (FAB run + step states) ---------- */
function statusKind(status) {
  switch (String(status || "").toLowerCase()) {
    case "passed": return "pass";
    case "failed": case "timed_out": return "fail";
    case "running": return "run";
    /* "queued" is Argo made a Pod for this and it has not started; "pending"
       is the pipeline declares this and execution has not reached it. Both
       read as not-yet-started to the eye, so they share a kind and a glyph —
       the label keeps them apart wherever the difference matters. */
    case "queued": case "pending": return "pending";
    case "interrupted": case "skipped": return "cancel";
    default: return "unknown";
  }
}
function statusLabel(status) {
  const s = String(status || "").toLowerCase();
  if (s === "timed_out") return "timed out";
  return s || "unknown";
}
function pill(label, kind) {
  kind = kind || statusKind(label);
  const dot = kind === "run" ? "live" : kind;
  return `<span class="pill ${kind}"><span class="dot ${dot}"></span>${esc(label || "unknown")}</span>`;
}
function statusIcon(status) {
  const kind = statusKind(status);
  const glyph = { pass: "✓", fail: "✗", cancel: "⊘", run: "◠", pending: "◌", unknown: "?" }[kind] || "?";
  return `<span class="ic ${kind}">${glyph}</span>`;
}
function bigIcon(status) {
  const kind = statusKind(status);
  const cls = { pass: "g", fail: "r", run: "o" }[kind] || "d";
  const glyph = { pass: "✓", fail: "✗", cancel: "⊘", run: "◠", pending: "◌", unknown: "?" }[kind] || "?";
  return `<span class="ic ${cls}">${glyph}</span>`;
}
function runStatusCell(run) {
  return `${statusIcon(run.Status)}${pill(statusLabel(run.Status), statusKind(run.Status))}`;
}
function releaseBadge(run) { return run.Release ? '<span class="v4b release">⏏ RELEASE</span>' : ""; }
function credentialedBadge(run) { return !run.Release && run.Credentialed ? '<span class="v4b release" title="credentialed CI run with secret-store access">⚿ CREDENTIALED</span>' : ""; }
function supersededBadge(run) {
  if (!run.SupersededBy) return "";
  return `<span class="v4b superseded" title="superseded by ${esc(run.SupersededBy)}">superseded</span>`;
}
function runDuration(run) {
  return spanSeconds(run.StartedAt, run.FinishedAt, run.Status === "running");
}
function runWhen(run) { return run.FinishedAt || run.StartedAt || run.QueuedAt || run.CreatedAt; }
function runMessage(run) {
  if (run.Error) return run.Error;
  if (run.FailedStep) return `failed at ${run.FailedBurn ? run.FailedBurn + " / " : ""}${run.FailedStep}`;
  if (run.SupersededBy) return `superseded by ${run.SupersededBy}`;
  if (run.Reason) return run.Reason;
  return "";
}
function repoName(repoID) {
  const repo = state.repos.find(r => r.ID === repoID);
  return repo ? repo.Name : (repoID ? "repo#" + repoID : "workspace");
}

/* ---------- chrome ---------- */
function setChrome(tab, sub) {
  for (const name of ["Runs", "Repos", "Issues", "Status"]) {
    const el = document.getElementById("tab" + name);
    if (el) el.classList.toggle("on", tab === name.toLowerCase());
  }
  const brand = document.getElementById("brandSub");
  if (brand) brand.textContent = sub || "/ dashboard";
}
function setConn(ok, msg) {
  const led = document.getElementById("connLed");
  const seg = document.querySelector(".backend");
  if (led) led.className = "led " + (ok ? "ok" : "bad");
  if (seg) { seg.title = String(msg || ""); seg.setAttribute("aria-label", String(msg || "")); }
}
function setVersion(ok, version) {
  const seg = document.getElementById("versionStatus"), label = document.getElementById("verLabel");
  const value = version || document.body.dataset.version || "dev";
  if (label) label.textContent = value;
  if (seg) {
    seg.className = "status-segment version-mode " + (ok ? "ok" : "unknown");
    const detail = ok ? `oberth ${value}` : `oberth ${value} (unconfirmed)`;
    seg.title = detail;
    seg.setAttribute("aria-label", detail);
  }
}
function initTheme() { if (localStorage.getItem("oberth-theme") === "light") document.body.classList.add("light"); }
function toggleTheme() {
  document.body.classList.toggle("light");
  localStorage.setItem("oberth-theme", document.body.classList.contains("light") ? "light" : "dark");
}

/* ---------- auth: token entered once, kept in localStorage ---------- */
function hasToken() { return !!localStorage.getItem("oberth-token"); }
function setToken(t) { if (t) localStorage.setItem("oberth-token", t.trim()); else localStorage.removeItem("oberth-token"); }
function authHeaders() { const token = localStorage.getItem("oberth-token"); return token ? { "Authorization": "Bearer " + token } : {}; }
function showTokenPrompt(message) {
  clearInterval(state.timer);
  replaceApp(`<section class="screen"><div class="table token-panel"><h1>Authentication required</h1><p class="meta">Enter the Oberth uplink bearer token. It is stored in this browser's localStorage so navigation and reloads never re-prompt.</p>${message ? `<p class="meta chain-bad">${esc(message)}</p>` : ""}<input id="tokenInput" type="password" placeholder="Bearer token" autocomplete="off"><div class="token-actions"><button class="chip on" data-action="submit-token">Connect</button></div></div></section>`);
  document.getElementById("tokenInput")?.focus();
}
function submitToken() {
  const value = document.getElementById("tokenInput")?.value;
  if (!value) return;
  state.authGen++;
  if (state.pendingController) { state.pendingController.abort(); state.pendingController = null; }
  setToken(value);
  route();
}
function clearToken() {
  state.authGen++;
  if (state.pendingController) { state.pendingController.abort(); state.pendingController = null; }
  setToken(null); setConn(false, "signed out"); showTokenPrompt();
}

async function api(url) {
  const gen = state.authGen;
  const seq = state.routeSeq;
  const controller = new AbortController();
  state.pendingController = controller;
  let response;
  try {
    response = await fetch(url, { cache: "no-store", headers: authHeaders(), signal: controller.signal });
  } catch (err) {
    if (err.name === "AbortError") throw err;
    setConn(false, "network unreachable");
    throw new Error("Oberth API is unreachable");
  }
  // Discard the response if auth changed or route changed while we waited.
  if (state.authGen !== gen || state.routeSeq !== seq) throw new Error("stale request");
  if (response.status === 401) {
    // Only show the prompt if auth generation has not changed since we sent.
    if (state.authGen === gen) showTokenPrompt(hasToken() ? "Token rejected — enter a current uplink token." : "");
    throw new Error("authentication required");
  }
  if (!response.ok) {
    let detail = "";
    try { detail = (await response.json()).error || ""; } catch { /* opaque body */ }
    const error = new Error(detail || "HTTP " + response.status);
    error.status = response.status;
    throw error;
  }
  setConn(true, "connected");
  return response.json();
}

/* ---------- navigation ---------- */
function go(path) {
  if (!location.pathname.startsWith("/runs/")) state.lastList = location.pathname;
  if (typeof history.pushState === "function") { history.pushState(null, "", path); route(); return; }
  location.href = path;
}
function currentRoute(seq) { return seq === state.routeSeq; }
function setAuto(ms) {
  clearInterval(state.timer);
  state.timer = setInterval(() => { route(true).catch(() => { }); }, ms || listPollMs);
}

/* ---------- data loading ---------- */
async function loadRepos() { state.repos = await api("/api/repos") || []; }
async function loadRuns() { state.runs = await api("/api/runs?limit=100") || []; }
async function loadRepoRuns() {
  if (state.repo === "all") { state.repoRuns = []; return; }
  try {
    state.repoRuns = await api(`/api/runs?limit=100&repo=${enc(state.repo)}`) || [];
  } catch (err) {
    if (err.status === 404) { state.repo = "all"; localStorage.setItem("oberth-repo", "all"); state.repoRuns = []; return; }
    throw err;
  }
}
async function loadStatus() {
  state.status = await api("/api/status");
  setVersion(true, state.status?.version);
}

/* ---------- runs list ---------- */
function runMatches(run) {
  if (state.repo !== "all" && repoName(run.RepoID) !== state.repo) return false;
  if (state.filter === "release") return !!run.Release;
  if (state.filter !== "all") return String(run.Status) === state.filter;
  return true;
}
function repoCounts(runs) {
  const counts = new Map([["all", runs.length]]);
  for (const run of runs) {
    const name = repoName(run.RepoID);
    counts.set(name, (counts.get(name) || 0) + 1);
  }
  return counts;
}
function runsTable(runs) {
  return `<div class="table runs-table"><div class="trow head"><div>status</div><div>run</div><div>sha</div><div>actor</div><div>when</div><div>message</div></div>${runs.map(run => `<div class="trow" data-run-id="${esc(run.ID)}"><div class="status-cell run-status">${runStatusCell(run)}</div><div class="primary"><div class="repo-name">${esc(repoName(run.RepoID))} ${releaseBadge(run)}${credentialedBadge(run)} ${supersededBadge(run)}</div><div class="branch">${esc(run.RefKind === "tag" ? "tag " : "")}${esc(run.Ref)}</div></div><div class="sha">${esc(shortSha(run.SHA))}</div><div class="clip" title="${esc(run.Actor)}">${esc(run.Actor || "--")}</div><div class="nowrap" title="${esc(fmtTime(runWhen(run)))}">${esc(ago(runWhen(run)))}<br><span class="meta">${esc(fmtDur(runDuration(run)))}</span></div><div class="clip" title="${esc(runMessage(run))}">${esc(runMessage(run))}</div></div>`).join("")}</div>`;
}
async function renderRuns(seq) {
  setChrome("runs", "/ runs");
  await Promise.all([loadRepos(), loadRuns(), loadRepoRuns()]);
  if (!currentRoute(seq)) return;
  localStorage.setItem("oberth-filter", state.filter);
  localStorage.setItem("oberth-repo", state.repo);
  const runs = state.runs, source = state.repo !== "all" ? state.repoRuns : runs;
  const visible = source.filter(runMatches), counts = repoCounts(runs);
  if (state.repo !== "all") counts.set(state.repo, state.repoRuns.length);
  const running = runs.filter(run => run.Status === "running").length;
  const queued = runs.filter(run => run.Status === "queued").length;
  const failed = runs.filter(run => run.Status === "failed").length;
  const releases = runs.filter(run => run.Release).length;
  replaceApp(`
  <section class="screen">
    <div class="bar">
      <h1>Runs</h1>
      <div class="stats"><span><b>${runs.length}</b> retained</span><span><b>${running}</b> running</span><span><b>${queued}</b> queued</span><span><b>${failed}</b> failed</span><span><b>${releases}</b> release</span></div>
    </div>
    <div class="runs-layout">
      <aside class="repo-side">
        <div class="repo-filter-row"><button class="repo-filter ${state.repo === "all" ? "on" : ""}" data-repo-filter="all"><span class="name">All repos</span><span class="count">${esc(counts.get("all") || 0)}</span></button></div>
        ${state.repos.map(repo => `<div class="repo-filter-row"><button class="repo-filter ${state.repo === repo.Name ? "on" : ""}" data-repo-filter="${esc(repo.Name)}"><span class="name">${esc(repo.Name)}</span><span class="count">${esc(counts.get(repo.Name) || 0)}</span></button></div>`).join("")}
      </aside>
      <section>
        <div class="toolbar">
          ${runFilters.map(f => `<button class="chip ${state.filter === f ? "on" : ""}" data-status-filter="${esc(f)}">${esc(f)}</button>`).join("")}
          <span class="count">${visible.length} shown</span>
        </div>
        ${visible.length ? runsTable(visible) : `<div class="empty">No runs match the current filters.</div>`}
      </section>
    </div>
  </section>`);
  setAuto(listPollMs);
}

/* ---------- run detail: burn/step breakdown + retained logs ---------- */
function stepKey(step) { return step.Burn + " / " + step.Step; }
function groupSteps(steps) {
  const burns = [];
  let current = null;
  for (const step of steps || []) {
    if (!current || current.name !== step.Burn) {
      current = { name: step.Burn, steps: [] };
      burns.push(current);
    }
    current.steps.push(step);
  }
  for (const burn of burns) {
    const kinds = burn.steps.map(step => statusKind(step.Status));
    burn.kind = kinds.includes("fail") ? "fail" : kinds.includes("run") ? "run" : kinds.includes("pending") ? "pending" : kinds.every(kind => kind === "cancel") ? "cancel" : "pass";
    burn.seconds = burn.steps.reduce((sum, step) => sum + spanSeconds(step.StartedAt, step.FinishedAt, false), 0);
  }
  return burns;
}
function defaultStep(detail) {
  const steps = detail.Steps || [];
  const failed = steps.find(step => statusKind(step.Status) === "fail");
  if (failed) return stepKey(failed);
  return "";
}
/* ---------- log line classification ----------
   Colour keys off structured markers, never a bare substring scan. This
   repository's own test names (TestValidateUsageErrors,
   TestExitCodeNeverReportsSuccessForAFailedNode), Go module paths
   (github.com/go-errors/errors) and the runner's own routine success line
   (msg="sub-process exited" error=<nil>) all carry failure words while
   reporting nothing wrong. Painting them red buries the line that does
   matter — the release-build log that ends in "Error: exit status 1".

   Precedence: bracket markers the dashboard writes itself, then hard
   markers (panic:/fatal:/Error:), then Go test verdicts, then structured
   logfmt fields, then a whole-word scan of the residual free text.

   Two rules carry most of the precision:
   - A Go test verdict is decided by the marker at line start. Everything
     after it is an arbitrary test NAME and is never scanned for keywords.
   - An error= field counts only when its value is neither empty nor a nil
     sentinel; a level= field is authoritative for its own line.

   Every pattern is RE2-compatible — no lookaround, no backreferences — so
   static_logclass_test.go compiles these exact sources with Go's regexp and
   replays real captured CI output through them. Keep it that way. */
const LOG_RULES = {
  stepPrefix: "^\\[[^\\]]*/[^\\]]*\\] ",
  markBad: "^\\[(fail|failed|error)\\]",
  markOk: "^\\[(pass|passed)\\]",
  markWarn: "^\\[(pending|warn|skip)\\]",
  hardBad: "^(panic:|fatal:|fatal error:|error:|err:|warning: data race)",
  testFail: "^(--- FAIL|=== FAIL|FAIL)([ \\t:]|$)",
  testSkip: "^(--- SKIP|=== SKIP)([ \\t:]|$)",
  testPass: "^(PASS|ok)([ \\t]|$)",
  testQuiet: "^(--- PASS|=== PASS|=== RUN|=== PAUSE|=== CONT|=== NAME)([ \\t:]|$)",
  errField: "(^|[\\s,;])(err|errs|error|errors)=(\"[^\"]*\"|'[^']*'|\\S*)",
  levelField: "(^|[\\s,;])(level|lvl|severity)=\"?([A-Za-z]+)\"?",
  wordBad: "(^|[^A-Za-z0-9._/-])(error|errors|failed|failure|fatal|panic|unavailable)([^A-Za-z0-9._/-]|$)",
  wordOk: "(^|[^A-Za-z0-9._/-])(success|succeeded|passed)([^A-Za-z0-9._/-]|$)",
  wordWarn: "(^|[^A-Za-z0-9._/-])(pending|waiting|queued|skipped|warn|warning)([^A-Za-z0-9._/-]|$)",
  info: "^([$] |run |step:)",
};
const LOG_RE = Object.fromEntries(Object.entries(LOG_RULES).map(([name, source]) => [name, new RegExp(source, "i")]));
const LOG_ERR_FIELD_ALL = new RegExp(LOG_RULES.errField, "gi");
/* An error= field carrying one of these is the emitter saying "no error". */
const LOG_NIL_VALUES = new Set(["", "\"\"", "''", "<nil>", "nil", "null", "none", "<none>", "-"]);
const LOG_LEVEL_BAD = new Set(["error", "err", "fatal", "crit", "critical", "panic", "emerg", "alert"]);
const LOG_LEVEL_WARN = new Set(["warn", "warning"]);
function logLineClass(line) {
  /* Classification-only prefix strip: anchored rules must work in the
     combined live view, where lines still carry [burn/step]. Display
     stripping is stripStepPrefix's job and is untouched by this. */
  const body = String(line || "").replace(LOG_RE.stepPrefix, "").trim();
  if (!body) return "";
  if (LOG_RE.markBad.test(body)) return "bad";
  if (LOG_RE.markOk.test(body)) return "ok";
  if (LOG_RE.markWarn.test(body)) return "warn";
  if (LOG_RE.hardBad.test(body)) return "bad";
  if (LOG_RE.testFail.test(body)) return "bad";
  if (LOG_RE.testSkip.test(body)) return "warn";
  if (LOG_RE.testPass.test(body)) return "ok";
  /* Per-test verdicts and RUN/PAUSE/CONT markers stay uncoloured: a green
     suite is hundreds of them, and a wall of green hides a red as well as a
     wall of red does. The package-level PASS/ok summary above carries the
     signal. This return is explicit so a test NAME never reaches the
     free-text scan below. */
  if (LOG_RE.testQuiet.test(body)) return "";
  const err = LOG_RE.errField.exec(body);
  if (err && !LOG_NIL_VALUES.has(String(err[3] || "").toLowerCase())) return "bad";
  const level = LOG_RE.levelField.exec(body);
  if (level) {
    const name = String(level[3] || "").toLowerCase();
    if (LOG_LEVEL_BAD.has(name)) return "bad";
    if (LOG_LEVEL_WARN.has(name)) return "warn";
    return "";
  }
  /* Structured error fields were judged above; remove them so their key
     name cannot re-trigger a free-text word match. */
  const residual = body.replace(LOG_ERR_FIELD_ALL, " ");
  if (LOG_RE.wordBad.test(residual)) return "bad";
  if (LOG_RE.wordOk.test(residual)) return "ok";
  if (LOG_RE.wordWarn.test(residual)) return "warn";
  if (LOG_RE.info.test(body)) return "info";
  return "";
}
function splitLogText(raw) {
  const clean = String(raw || "").replaceAll("\r\n", "\n").replaceAll("\r", "\n");
  if (!clean) return [];
  const lines = clean.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}
/* stripStepPrefix removes the [burn/step] marker from each line when the
   viewer is already scoped to that exact step. The prefix is the literal
   string the runner writes (copyPrefixed) and that runlog indexes on; it
   stays in the stored data and the API response — this strips it only for
   the human-facing single-step terminal view. */
function stripStepPrefix(lines, burn, step) {
  const prefix = "[" + burn + "/" + step + "] ";
  return lines.map(line => line.startsWith(prefix) ? line.slice(prefix.length) : line);
}
const maxRenderedLogLines = 5000;
function renderLogLines(lines) {
  const total = lines.length;
  const truncated = total > maxRenderedLogLines;
  const start = truncated ? total - maxRenderedLogLines : 0;
  let out = "";
  if (truncated) {
    out += `<div class="log-line"><span class="log-no">--</span><span class="log-txt warn">[truncated] showing last ${maxRenderedLogLines} of ${total} lines</span></div>`;
  }
  for (let i = start; i < total; i++) {
    out += `<div class="log-line"><span class="log-no">${i + 1}</span><span class="log-txt ${logLineClass(lines[i])}">${esc(lines[i]) || "&nbsp;"}</span></div>`;
  }
  return out;
}
function summaryLines(detail) {
  const run = detail.Run || {}, steps = detail.Steps || [];
  const lines = [
    `$ oberth run ${repoName(run.RepoID)}/${run.Ref}`,
    `run ${run.ID} status=${statusLabel(run.Status)} sha=${shortSha(run.SHA)} trigger=${run.Trigger || "--"}`,
    "",
  ];
  const passed = steps.filter(step => step.Status === "passed").length;
  const burns = new Set(steps.map(step => step.Burn)).size;
  /* Steps the pipeline declares that this run never reached. They exist in the
     list because the plan is seeded at submission; counting them is the whole
     point of seeding it — "12 of 16" is a different fact from "12 passed". */
  const unreached = steps.filter(step => step.Status === "pending").length;
  const reached = steps.length - unreached;
  if (run.Release) lines.push("[release] reachable-tag release run — release burns executed with release credentials");
  else if (run.Credentialed) lines.push("[credentialed] CI run with approved secret-store access — secrets are scoped per-repo, audit-attributed; in-step redaction via oberth-secret-materialize");
  const kind = statusKind(run.Status);
  if (kind === "pass") lines.push(`[pass] ${passed} step${passed === 1 ? "" : "s"} passed across ${burns} burn${burns === 1 ? "" : "s"}`);
  else if (kind === "fail") lines.push(`[fail] failed at ${run.FailedBurn ? run.FailedBurn + " / " : ""}${run.FailedStep || "unknown step"} — ${passed} step${passed === 1 ? "" : "s"} passed`);
  else if (kind === "run") lines.push(`[pending] running in phase ${run.Phase || "ci"} — ${reached} of ${steps.length} step${steps.length === 1 ? "" : "s"} started, ${passed} passed`);
  else if (kind === "cancel") lines.push(`[interrupted] ${run.Reason || "run was interrupted"}`);
  else lines.push(`[pending] queued behind earlier work for this repository`);
  if (unreached) lines.push(`${unreached} declared step${unreached === 1 ? "" : "s"} not reached`);
  if (run.SupersededBy) lines.push(`superseded by ${run.SupersededBy}`);
  if (run.Error) lines.push("", `error: ${run.Error}`);
  const timed = steps.map(step => ({ step, seconds: spanSeconds(step.StartedAt, step.FinishedAt, false) })).filter(item => item.seconds > 0).sort((a, b) => b.seconds - a.seconds);
  if (timed.length) lines.push("", `slowest: ${timed.slice(0, 2).map(item => `${stepKey(item.step)} ${fmtDur(item.seconds)}`).join(" · ")}`);
  lines.push("", "select a step for its retained log");
  return lines;
}
async function stepLogContent(runID, step, active) {
  if (step.Status === "pending") return renderLogLines([`step: ${stepKey(step)} · pending`, "", "step is declared by the pipeline and has not been reached yet"]);
  if (statusKind(step.Status) === "pending") return renderLogLines([`step: ${stepKey(step)} · queued`, "", "step is waiting for its turn"]);
  if (statusKind(step.Status) === "run") return renderLogLines([`step: ${stepKey(step)} · running`, "", "step is running — the retained log appears when the step completes"]);
  const cacheKey = `${runID}|${step.Burn}|${step.Step}|${step.FinishedAt || ""}`;
  if (state.logCache.has(cacheKey)) return state.logCache.get(cacheKey);
  let html;
  try {
    const payload = await api(`/api/runs/${enc(runID)}/logs?burn=${enc(step.Burn)}&step=${enc(step.Step)}`);
    const lines = stripStepPrefix(splitLogText(payload.output), step.Burn, step.Step);
    html = renderLogLines([`step: ${stepKey(step)} · ${statusLabel(step.Status)}${step.ExitCode ? ` · exit ${step.ExitCode}` : ""}`, ""].concat(lines.length ? lines : ["no retained step output"]));
  } catch (err) {
    html = renderLogLines([`step: ${stepKey(step)} · ${statusLabel(step.Status)}`, "", `step log unavailable: ${err.message || err}`]);
  }
  if (state.logCache.size > 32) state.logCache.clear();
  state.logCache.set(cacheKey, html);
  return html;
}
/* Live tail of a running Job's redacted log stream. Each poll appends the
   server's next chunk to a per-run buffer; the buffer is bounded so an hour of
   fast output cannot grow the tab without limit. */
async function liveLogContent(run, activeBurn, activeStep) {
  // Capture a local reference so a late response cannot corrupt a different
  // run's state. Each run gets its own live object; switching runs abandons
  // the old one in-flight.
  if (!state.live || state.live.runID !== run.ID) {
    state.live = { runID: run.ID, offset: -1, text: "", busy: false };
  }
  const liveState = state.live;
  if (!liveState.busy) {
    liveState.busy = true;
    try {
      const payload = await api(`/api/runs/${enc(run.ID)}/livelog?offset=${liveState.offset}`);
      // Apply the response only if this live state is still current.
      if (state.live === liveState) {
        if (payload.chunk) liveState.text += payload.chunk;
        if (typeof payload.offset === "number" && payload.offset > 0) liveState.offset = payload.offset;
        if (liveState.text.length > 400000) liveState.text = liveState.text.slice(-300000);
      }
    } catch { /* transient poll failure: keep showing the buffer */
    } finally { liveState.busy = false; }
  }
  let lines = splitLogText(liveState.text);
  if (activeBurn && activeStep) lines = stripStepPrefix(lines, activeBurn, activeStep);
  return renderLogLines([`$ oberth live · ${repoName(run.RepoID)}/${run.Ref} · phase ${run.Phase || "ci"}`, ""].concat(
    lines.length ? lines : ["waiting for runner output…"]));
}
async function renderRunDetail(runID, seq, background) {
  if (!background) setChrome("runs", "/ run");
  if (!state.repos.length) { try { await loadRepos(); } catch { /* repo names fall back to IDs */ } }
  let detail;
  try {
    detail = await api(`/api/runs/${enc(runID)}`);
  } catch (err) {
    if (!currentRoute(seq)) return;
    replaceApp(`<section class="screen"><div class="error">Run ${esc(runID)} is unavailable: ${esc(err.message || err)}</div></section>`);
    return;
  }
  if (!currentRoute(seq)) return;
  await renderRunDetailView(detail, seq);
}
async function renderRunDetailView(detail, seq) {
  const run = detail.Run || {}, steps = detail.Steps || [];
  const repository = detail.Repository && detail.Repository.Name ? detail.Repository.Name : repoName(run.RepoID);
  if (state.stepRunID !== run.ID) { state.step = ""; state.stepRunID = run.ID; }
  const keys = steps.map(stepKey);
  if (state.step && !keys.includes(state.step)) state.step = "";
  const selectedKey = state.step || defaultStep(detail);
  const selected = steps.find(step => stepKey(step) === selectedKey) || null;
  const burns = groupSteps(steps);
  const kind = statusKind(run.Status);
  const phaseLabel = statusLabel(run.Status).toUpperCase();
  const seconds = runDuration(run);
  const stepCount = steps.length;
  // A running run streams live output in the terminal: on the summary view and
  // on any still-running step. Completed steps keep their retained slice.
  const live = kind === "run" && (!selected || statusKind(selected.Status) === "run");
  const terminal = live ? await liveLogContent(run, selected?.Burn, selected?.Step) : selected ? await stepLogContent(run.ID, selected, kind === "run") : renderLogLines(summaryLines(detail));
  if (!currentRoute(seq)) return;
  // Preserve the reader's place across the poll re-render: stick to the
  // bottom while they are at the bottom, hold their position when they have
  // scrolled up to read.
  const previousTerm = document.getElementById("termBody");
  const stick = !previousTerm || previousTerm.scrollHeight - previousTerm.scrollTop - previousTerm.clientHeight < 40;
  const previousScroll = previousTerm ? previousTerm.scrollTop : 0;
  const previousStepList = document.getElementById("stepList");
  const previousStepScroll = previousStepList ? previousStepList.scrollTop : 0;
  setChrome("runs", `/ ${repository}`);
  replaceApp(`
  <section class="screen wide">
    <div class="rh">
      <div class="rh1">
        ${bigIcon(run.Status)}
        <span class="rh-name"><span class="rp">${esc(repository)} /</span> ${esc(run.Ref)}</span>
        <span class="v4b ${kind === "pass" ? "pass" : kind === "fail" ? "fail" : kind === "run" ? "run" : "dim"}">${esc(phaseLabel)}</span>
        ${releaseBadge(run)}${credentialedBadge(run)}
        ${run.SupersededBy ? '<span class="v4b superseded">SUPERSEDED</span>' : ""}
        <div class="time"><span><b>${esc(fmtDur(seconds))}</b></span><span>${esc(ago(runWhen(run)))}</span><button class="v4back" data-action-back>‹ back</button></div>
      </div>
      <div class="rh2">
        <span>actor <span class="ag" title="${esc(run.Actor)}">${esc(run.Actor || "--")}</span></span>
        <span>sha <span class="sha cpy" data-copy-text="${esc(run.SHA)}" title="${esc(run.SHA)} — click to copy">${esc(shortSha(run.SHA))}</span></span>
        <span class="cpy" data-copy-text="${esc(run.ID)}" title="${esc(run.ID)} — click to copy">run ${esc(shortId(run.ID))} ⧉</span>
        <span>trigger ${esc(run.Trigger || "--")}</span>
        <span>${esc(run.RefKind === "tag" ? "tag" : "branch")}</span>
        <span>queued ${esc(fmtTime(run.QueuedAt))}</span>
        ${run.FinishedAt ? `<span>finished ${esc(fmtTime(run.FinishedAt))}</span>` : ""}
      </div>
    </div>
    ${kind === "fail" ? `<div class="fbanner"><b>✗ ${esc(run.FailedStep ? (run.FailedBurn ? run.FailedBurn + " / " : "") + run.FailedStep : "run failed")}</b><span class="err" title="${esc(run.Error)}">${esc(run.Error || "")}</span></div>` : ""}
    ${run.SupersededBy ? `<div class="sbanner"><b>superseded</b><span class="err">a newer push replaced this run</span><button class="chip" data-run-id="${esc(run.SupersededBy)}">view successor ${esc(shortId(run.SupersededBy))}</button></div>` : ""}
    <div class="v4-body">
      <div class="slist" id="stepList">
        <div class="sl-h"><span>STEPS · ${stepCount}${run.Release ? " · RELEASE" : run.Credentialed ? " · CREDENTIALED" : ""}</span></div>
        <button class="v4row summary${selected ? "" : " sel"}" data-select-step=""><span class="ic ${kind === "pass" ? "g" : kind === "fail" ? "r" : kind === "run" ? "o" : "d"}">≡</span><span class="nm">Run summary</span><span class="dur">${esc(fmtDur(seconds))}</span></button>
        <div class="sl-sep"></div>
        ${burns.map(burn => { const solo = burn.steps.length === 1 && burn.steps[0].Step.toLowerCase() === burn.name.toLowerCase(); const header = solo ? "" : `<div class="burn-h">${statusIcon(burn.kind === "pass" ? "passed" : burn.kind === "fail" ? "failed" : burn.kind === "run" ? "running" : burn.kind === "pending" ? "queued" : "skipped")}<span>${esc(burn.name)}</span><span class="dur">${esc(fmtDur(burn.seconds))}</span></div>`; return header + burn.steps.map(step => { const key = stepKey(step), sk = statusKind(step.Status); return `<button class="v4row${key === selectedKey && selected ? " sel" : ""}${sk === "pending" ? " pend" : ""}" data-select-step="${esc(key)}"><span class="ic ${sk === "pass" ? "g" : sk === "fail" ? "r" : sk === "run" ? "o" : "d"}">${sk === "pass" ? "✓" : sk === "fail" ? "✗" : sk === "run" ? "◠" : sk === "cancel" ? "⊘" : "◌"}</span><span class="nm">${esc(step.Step)}</span><span class="dur">${sk === "cancel" ? "skipped" : sk === "pending" ? esc(statusLabel(step.Status)) : esc(fmtDur(spanSeconds(step.StartedAt, step.FinishedAt, sk === "run")))}</span></button>`; }).join(""); }).join("") || '<div class="empty">No step results recorded yet.</div>'}
      </div>
      <div class="logp">
        <div class="log-h" id="logHead">${selected ? `${statusIcon(selected.Status)}<span class="nm">${esc(stepKey(selected))}</span><span class="v4b ${statusKind(selected.Status) === "pass" ? "pass" : statusKind(selected.Status) === "fail" ? "fail" : statusKind(selected.Status) === "run" ? "run" : "dim"}">${esc(statusLabel(selected.Status).toUpperCase())}</span>` : `<span class="ic ${kind === "pass" ? "g" : kind === "fail" ? "r" : kind === "run" ? "o" : "d"}">≡</span><span class="nm">Run summary</span><span class="v4b ${kind === "pass" ? "pass" : kind === "fail" ? "fail" : kind === "run" ? "run" : "dim"}">${esc(phaseLabel)} · ${esc(fmtDur(seconds))}</span>`}</div>
        <div class="terminal"><div class="term-bar"><span class="lamp red"></span><span class="lamp yellow"></span><span class="lamp green"></span><span class="term-title">${esc(repository)} / ${esc(run.Ref)} — ${live ? "live output" : selected ? esc(stepKey(selected)) : "summary"}</span>${live ? '<span class="term-live"><span class="dot live"></span>LIVE</span>' : ""}</div><div id="termBody" class="term-body">${terminal}</div></div>
      </div>
    </div>
  </section>`);
  const termBody = document.getElementById("termBody");
  if (termBody && (live || selected)) termBody.scrollTop = stick ? termBody.scrollHeight : previousScroll;
  const stepList = document.getElementById("stepList");
  if (stepList) stepList.scrollTop = previousStepScroll;
  if (run.Status === "running") setAuto(livePollMs);
  else if (run.Status === "queued") setAuto(listPollMs);
  else clearInterval(state.timer);
}

/* ---------- repositories ---------- */
function upstreamName(upstreamID) {
  const info = (state.status?.upstream_info || []).find(upstream => upstream.id === upstreamID);
  return info ? info.name : (upstreamID ? "#" + upstreamID : "--");
}
function historyStrip(runs) {
  return `<div class="history" aria-label="recent runs, oldest to newest">${runs.slice(0, 12).reverse().map(run => `<button type="button" class="hist ${statusKind(run.Status)}" data-run-id="${esc(run.ID)}" title="${esc(`${shortSha(run.SHA)} · ${run.RefKind === "tag" ? "tag" : "branch"} ${run.Ref} · ${statusLabel(run.Status)} · ${ago(runWhen(run))}`)}"></button>`).join("") || '<span class="meta">--</span>'}</div>`;
}
async function renderRepos(seq) {
  setChrome("repos", "/ repos");
  await Promise.all([loadRepos(), loadStatus().catch(() => { })]);
  if (!currentRoute(seq)) return;
  const rows = state.repos.map(repo => {
    const runs = repo.LatestRuns || [];
    return { repo, runs, latest: runs[0] || null };
  });
  replaceApp(`
  <section class="screen">
    <div class="bar"><h1>Repositories</h1><span class="meta">${rows.length} configured</span></div>
    <div class="table repo-table">
      <div class="trow head"><div>repository</div><div>status</div><div>default branch</div><div>latest run</div><div>history</div><div>upstream</div></div>
      ${rows.map(({ repo, runs, latest }) => `<div class="trow" data-repo-runs="${esc(repo.Name)}"><div class="primary"><div class="repo-name">${esc(repo.Name)}</div></div><div>${latest ? pill(statusLabel(latest.Status), statusKind(latest.Status)) : '<span class="meta">no runs</span>'}</div><div class="branch">${esc(repo.DefaultBranch || "--")}</div><div>${latest ? `<div class="branch">${esc(latest.Ref)}</div><span class="meta">${esc(ago(runWhen(latest)))}</span>` : '<span class="meta">--</span>'}</div><div>${historyStrip(runs)}</div><div class="clip">${esc(upstreamName(repo.UpstreamID))}</div></div>`).join("") || '<div class="empty static">No repositories registered. Register an upstream and repository in-pod with oberth admin.</div>'}
    </div>
  </section>`);
  setAuto(slowPollMs);
}

/* ---------- issues (Oberth internal ledger) ---------- */
function issueListURL() {
  const query = new URLSearchParams({ limit: "50" });
  if (state.issueRepo) query.set("repo", state.issueRepo);
  if (state.issueKind) query.set("kind", state.issueKind);
  if (state.issueState) query.set("state", state.issueState);
  if (state.issueCursor) query.set("before", String(state.issueCursor));
  return "/api/issues?" + query.toString();
}
function issueRow(issue) {
  return `<button type="button" class="issue-row" data-open-issue="${esc(issue.ID)}" aria-label="Open internal issue ${esc(issue.ID)}: ${esc(issue.Title)}"><span class="sha">#${esc(issue.ID)}</span><span>${pill(issue.Kind, issue.Kind === "ci" ? "run" : "pending")}</span><span>${pill(issue.State, issue.State === "open" ? "fail" : "pass")}</span><span class="clip">${esc(issue.RepoID ? repoName(issue.RepoID) : "workspace")}</span><span class="branch clip">${esc(issue.Branch || "--")}</span><span class="issue-title" title="${esc(issue.Title)}">${esc(issue.Title)}</span><span title="occurrences">×${esc(issue.Occurrences || 1)}</span><span title="updated ${esc(fmtTime(issue.UpdatedAt))}">${esc(ago(issue.UpdatedAt))}</span></button>`;
}
async function renderIssues(seq) {
  setChrome("issues", "/ issues");
  if (!state.repos.length) { try { await loadRepos(); } catch { /* names fall back */ } }
  let page;
  try {
    page = await api(issueListURL());
  } catch (err) {
    if (!currentRoute(seq)) return;
    replaceApp(`<section class="screen"><h1>Issues</h1><div class="error">${esc(err.message || err)}<br><button type="button" class="retry" data-action="issue-retry">Retry</button></div></section>`);
    return;
  }
  if (!currentRoute(seq)) return;
  state.issuePage = page;
  const issues = page.Issues || [];
  const kinds = [["", "all"], ["ci", "ci"], ["manual", "manual"]];
  const states = [["open", "open"], ["closed", "closed"], ["", "all"]];
  replaceApp(`
  <section class="screen">
    <div class="bar"><h1>Issues</h1><span class="meta">Oberth internal ledger — CI failures and agent-filed work items</span></div>
    <div class="toolbar">
      ${kinds.map(([value, label]) => `<button class="chip ${state.issueKind === value ? "on" : ""}" data-issue-kind="${esc(value)}">${esc(label)}</button>`).join("")}
      <span class="status-divider"></span>
      ${states.map(([value, label]) => `<button class="chip ${state.issueState === value ? "on" : ""}" data-issue-state="${esc(value)}">${esc(label)}</button>`).join("")}
      <label>repo <select data-action="issue-repo"><option value="">all</option>${state.repos.map(repo => `<option value="${esc(repo.Name)}" ${state.issueRepo === repo.Name ? "selected" : ""}>${esc(repo.Name)}</option>`).join("")}</select></label>
      <span class="count">${issues.length} shown${state.issueCursorHistory.length ? ` · page ${state.issueCursorHistory.length + 1}` : ""}</span>
    </div>
    ${issues.length ? `<div class="table issues-table"><div class="issue-row head static"><span>id</span><span>kind</span><span>state</span><span>repo</span><span>branch</span><span>title</span><span>seen</span><span>updated</span></div>${issues.map(issueRow).join("")}</div>` : '<div class="empty">No internal issues match these filters.</div>'}
    <div class="issue-pager">
      <button type="button" data-action="issue-newer" ${state.issueCursorHistory.length ? "" : "disabled"}>‹ newer</button>
      <button type="button" data-action="issue-older" ${page.NextBefore ? "" : "disabled"}>older ›</button>
    </div>
  </section>`);
  setAuto(slowPollMs);
}
function changeIssueFilter(patch) {
  Object.assign(state, patch);
  state.issueCursor = 0;
  state.issueCursorHistory = [];
  route();
}
function openIssue(id, opener) {
  const issue = (state.issuePage?.Issues || []).find(item => String(item.ID) === String(id));
  if (!issue) return;
  state.issueOpener = opener || document.activeElement;
  const dialog = document.getElementById("issueDialog");
  document.getElementById("issueDialogTitle").textContent = `Issue #${issue.ID}`;
  document.getElementById("issueDialogMeta").textContent = `${issue.Kind} · ${issue.State}`;
  document.getElementById("issueDialogContent").innerHTML = `
    <h3>${esc(issue.Title || "Untitled issue")}</h3>
    <p class="meta">${esc(issue.RepoID ? repoName(issue.RepoID) : "workspace-global")} ${issue.Branch ? "/ " + esc(issue.Branch) : ""} · Oberth internal ledger</p>
    <dl class="kv-grid">
      <dt>kind</dt><dd>${esc(issue.Kind)}</dd>
      <dt>state</dt><dd>${esc(issue.State)}</dd>
      <dt>occurrences</dt><dd>${esc(issue.Occurrences || 1)}</dd>
      ${issue.CIOrigin ? `<dt>ci origin</dt><dd>${esc(issue.CIOrigin)}</dd>` : ""}
      ${issue.CIWorkID ? `<dt>ci work</dt><dd>${esc(issue.CIWorkID)}</dd>` : ""}
      <dt>created</dt><dd>${esc(fmtTime(issue.CreatedAt))}</dd>
      <dt>updated</dt><dd>${esc(fmtTime(issue.UpdatedAt))}</dd>
      ${issue.ClosedAt ? `<dt>closed</dt><dd>${esc(fmtTime(issue.ClosedAt))}</dd>` : ""}
    </dl>
    <h4>Body</h4>
    <div class="issue-body md">${issue.Body ? renderMarkdown(issue.Body) : esc("(empty)")}</div>`;
  document.querySelector(".app").inert = true;
  dialog.showModal();
}
function closeIssue() {
  const dialog = document.getElementById("issueDialog");
  if (dialog?.open) dialog.close();
  document.querySelector(".app").inert = false;
  const opener = state.issueOpener;
  state.issueOpener = null;
  if (opener?.isConnected) opener.focus();
}

/* ---------- status ---------- */
function ledFor(value) { return value === "ready" ? "ok" : "err"; }
function svcCard(name, value, sub, mood) {
  const cls = mood || (value === "ready" ? "ok" : "bad");
  return `<div class="svc ${cls}"><div class="svc-name"><span class="led ${cls === "ok" ? "ok" : cls === "warn" ? "" : "bad"}"></span>${esc(name)}</div><div class="svc-val">${esc(value)}</div>${sub ? `<div class="svc-sub">${sub}</div>` : ""}</div>`;
}
async function renderStatus(seq) {
  setChrome("status", "/ status");
  await Promise.all([loadStatus(), loadRepos().catch(() => { })]);
  if (!currentRoute(seq)) return;
  const s = state.status || {};
  const store = s.secret_store;
  const chain = s.audit_chain;
  const storeMood = !store || !store.configured ? "warn" : store.transport === "insecure-http" ? "warn" : store.probe && store.probe !== "ready" ? "bad" : "ok";
  const storeValue = !store || !store.configured ? "not configured" : store.transport === "insecure-http" ? "insecure http" : store.probe === "ready" ? "healthy" : store.probe === "sealed" ? "sealed" : store.probe ? "unhealthy" : "configured";
  replaceApp(`
  <section class="screen">
    <div class="bar"><h1>Status</h1><span class="meta">FAB control plane health</span></div>
    <div class="svc-grid">
      ${svcCard("database", s.database || "unavailable", `${esc(s.upstreams ?? 0)} upstream${s.upstreams === 1 ? "" : "s"} · ${esc(s.repositories ?? 0)} repos`)}
      ${svcCard("upstream vcs", s.vcs || "unavailable", "")}
      ${svcCard("kubernetes", s.cluster || "unavailable", "")}
      ${svcCard("audit anchor", s.audit || "unavailable", chain && chain.detail ? esc(chain.detail) : s.audit_mode === "local" ? "local hash chain — external anchoring off" : "")}
      ${svcCard("secret store", storeValue, store && store.address ? esc(store.address) : "OpenBao release secrets disabled", storeMood)}
      ${svcCard("server", s.version || document.body.dataset.version || "dev", "", "ok")}
    </div>
    <div class="vcs-panel">
      <div class="vcs-head">Upstreams</div>
      ${(s.upstream_info || []).map(upstream => `<div class="vcs-entry"><span class="vcs-led ${upstream.probe === "ready" ? "ok" : "err"}"></span><span class="vcs-host">${esc(upstream.name)}</span><span class="meta">${esc(upstream.kind)}</span><span class="mono-detail">${esc(upstream.base_url)}</span><span class="mono-detail">${esc(upstream.probe === "ready" ? "reachable" : upstream.probe || "unprobed")}</span></div>`).join("") || '<div class="vcs-entry"><span class="vcs-led na"></span><span class="meta">No upstream registered — run oberth admin upstream register in-pod.</span></div>'}
      ${s.ssh_identity ? `<div class="vcs-entry"><span class="vcs-led ok"></span><span class="vcs-host">SSH identity</span><span class="meta">upstream auth</span><span class="mono-detail">${esc(s.ssh_identity)}</span><span><button class="copy-btn" data-copy-text="${esc(s.ssh_identity)}">Copy</button></span></div>` : ""}
    </div>
    <div class="vcs-panel">
      <div class="vcs-head">Audit chain</div>
      ${chain ? `
      <div class="vcs-entry"><span class="vcs-led ${ledFor(s.audit)}"></span><span class="vcs-host">head</span><span class="meta">#${esc(chain.head_id ?? 0)}</span><span class="mono-detail">${esc((chain.head_sha256 || "").slice(0, 16))}${chain.head_sha256 ? "…" : ""}</span><span class="mono-detail">hash-chained actions</span></div>
      <div class="vcs-entry"><span class="vcs-led ${chain.anchor_id ? "ok" : "na"}"></span><span class="vcs-host">checkpoint</span><span class="meta">${chain.anchor_id ? "#" + esc(chain.anchor_id) : "none"}</span><span class="mono-detail">${esc(chain.tsa_url || "--")}</span><span class="mono-detail">${chain.anchored_at ? "anchored " + esc(ago(chain.anchored_at)) : chain.anchored === false ? "local mode — external anchoring disabled" : "not yet anchored"}</span></div>
      ${chain.detail ? `<div class="vcs-entry"><span class="vcs-led err"></span><span class="vcs-host chain-bad">attention</span><span class="mono-detail chain-bad" title="${esc(chain.detail)}">${esc(chain.detail)}</span><span></span><span></span></div>` : ""}` : `<div class="vcs-entry"><span class="vcs-led na"></span><span class="meta">Audit chain detail is unavailable.</span></div>`}
    </div>
    ${store && store.configured ? `
    <div class="vcs-panel">
      <div class="vcs-head">Secret store (OpenBao)</div>
      <div class="vcs-entry"><span class="vcs-led ${store.transport === "insecure-http" ? "err" : "ok"}"></span><span class="vcs-host">address</span><span class="meta">${esc(store.transport)}</span><span class="mono-detail">${esc(store.address)}</span><span class="mono-detail">auth ${esc(store.auth_mount)} · role ${esc(store.role)}</span></div>
      <div class="vcs-entry"><span class="vcs-led ${store.probe === "ready" ? "ok" : store.probe ? "err" : "na"}"></span><span class="vcs-host">verify</span><span class="meta">${store.probe === "ready" ? "login OK" : store.probe === "sealed" ? "SEALED" : store.probe ? "login failed" : "no probe"}</span><span class="mono-detail">${store.probe === "sealed" ? "sealed \u2014 unseal required; credentialed releases fail until unsealed" : store.probe && store.probe !== "ready" ? esc(store.probe) : "SA token \u2192 K8s-auth login \u2192 TokenReview \u2192 logout"}</span><span class="mono-detail">proves reachability, TLS/CA, unsealed, auth mount, role binding</span></div>
    </div>` : ""}
  </section>`);
  setAuto(slowPollMs);
}

/* ---------- router ---------- */
async function route(background) {
  const seq = ++state.routeSeq;
  clearInterval(state.timer);
  if (!hasToken()) { showTokenPrompt(); return; }
  if (!background) window.scrollTo(0, 0);
  try {
    const path = location.pathname;
    if (path === "/" || path === "/runs") return await renderRuns(seq);
    if (path === "/repos") return await renderRepos(seq);
    if (path === "/issues") return await renderIssues(seq);
    if (path === "/status") return await renderStatus(seq);
    const match = path.match(/^\/runs\/([^/]+)$/);
    if (match) return await renderRunDetail(decodeURIComponent(match[1]), seq, background);
    go("/runs");
  } catch (err) {
    if (!currentRoute(seq)) return;
    if (String(err.message) === "authentication required") return;
    setConn(false, String(err.message || err));
    replaceApp(`<section class="screen"><div class="error">${esc(err.message || err)}</div></section>`);
  }
}

/* ---------- events ---------- */
document.addEventListener("click", event => {
  const target = event.target instanceof Element ? event.target.closest("[data-route],[data-action]") : null;
  if (!target) return;
  if (target.dataset.route) { go(target.dataset.route); return; }
  switch (target.dataset.action) {
    case "toggle-theme": toggleTheme(); break;
    case "refresh": state.logCache.clear(); route(); break;
    case "clear-token": clearToken(); break;
    case "submit-token": submitToken(); break;
    case "close-issue": closeIssue(); break;
    case "issue-retry": route(); break;
    case "issue-older": {
      const next = state.issuePage?.NextBefore;
      if (next) { state.issueCursorHistory.push(state.issueCursor); state.issueCursor = next; route(); }
      break;
    }
    case "issue-newer": {
      if (state.issueCursorHistory.length) { state.issueCursor = state.issueCursorHistory.pop(); route(); }
      break;
    }
  }
});
document.addEventListener("change", event => {
  const target = event.target;
  if (!(target instanceof HTMLSelectElement)) return;
  if (target.dataset.action === "issue-repo") changeIssueFilter({ issueRepo: target.value });
});
app.addEventListener("click", event => {
  if (!(event.target instanceof Element)) return;
  let target;
  if ((target = event.target.closest("[data-copy-text]"))) {
    navigator.clipboard?.writeText(target.dataset.copyText).then(() => {
      const original = target.textContent;
      target.textContent = "copied ⧉";
      setTimeout(() => { if (target.isConnected) target.textContent = original; }, 1500);
    }).catch(() => { });
    return;
  }
  if ((target = event.target.closest("[data-open-issue]"))) { openIssue(target.dataset.openIssue, target); return; }
  /* data-run-id before data-repo-runs is load-bearing: history-strip buttons
     carry data-run-id inside a repo row that carries data-repo-runs — reversing
     the order would swallow the click and navigate to the filtered list instead
     of the run detail page. */
  if ((target = event.target.closest("[data-run-id]"))) { go(`/runs/${enc(target.dataset.runId)}`); return; }
  if ((target = event.target.closest("[data-repo-runs]"))) { state.repo = target.dataset.repoRuns; go("/runs"); return; }
  if ((target = event.target.closest("[data-repo-filter]"))) { state.repo = target.dataset.repoFilter; route(); return; }
  if ((target = event.target.closest("[data-status-filter]"))) { state.filter = target.dataset.statusFilter; route(); return; }
  if ((target = event.target.closest("[data-issue-kind]"))) { changeIssueFilter({ issueKind: target.dataset.issueKind }); return; }
  if ((target = event.target.closest("[data-issue-state]"))) { changeIssueFilter({ issueState: target.dataset.issueState }); return; }
  if ((target = event.target.closest("[data-select-step]")) && target.dataset.selectStep !== undefined) {
    state.step = target.dataset.selectStep;
    route(true);
    return;
  }
  if (event.target.closest("[data-action-back]")) { go(state.lastList || "/runs"); }
});
document.addEventListener("keydown", event => {
  if (event.key === "Enter" && event.target instanceof HTMLInputElement && event.target.id === "tokenInput") { submitToken(); return; }
  if (document.getElementById("issueDialog")?.open) return;
  if (event.target && ["INPUT", "SELECT", "TEXTAREA"].includes(event.target.tagName)) return;
  if (event.key === "r" || event.key === "R") route();
  if (event.key === "t" || event.key === "T") toggleTheme();
  if (event.key === "1") go("/runs");
  if (event.key === "2") go("/repos");
  if (event.key === "3") go("/issues");
  if (event.key === "4") go("/status");
  if (event.key === "Escape" && location.pathname.startsWith("/runs/")) go(state.lastList || "/runs");
  if ((event.key === "j" || event.key === "k") && location.pathname.startsWith("/runs/")) {
    const rows = [...document.querySelectorAll("#stepList [data-select-step]")];
    if (!rows.length) return;
    const current = rows.findIndex(row => row.classList.contains("sel"));
    const next = event.key === "j" ? Math.min(current + 1, rows.length - 1) : Math.max(current - 1, 0);
    if (next !== current) { state.step = rows[next].dataset.selectStep; route(true); }
  }
});
window.addEventListener("popstate", () => route());
const issueDialog = document.getElementById("issueDialog");
issueDialog?.addEventListener("cancel", event => { event.preventDefault(); closeIssue(); });
issueDialog?.addEventListener("click", event => { if (event.target === issueDialog) closeIssue(); });

/* ---------- boot ---------- */
initTheme();
setVersion(false, document.body.dataset.version);
route();
