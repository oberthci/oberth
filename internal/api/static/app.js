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
  runCounts: null, runCursor: 0, runCursorHistory: [], runStart: 1, runStartHistory: [], runSourceCount: 0,
  runNext: 0, runsNext: "", repoRunsNext: "",
  timer: null, routeSeq: 0,
  filter: readRunFilter(),
  releaseOnly: localStorage.getItem("oberth-kind") === "release" || localStorage.getItem("oberth-filter") === "release",
  repo: localStorage.getItem("oberth-repo") || "all",
  step: "", stepRunID: "",
  logCache: new Map(),
  live: null, // assigned per-run by liveLogContent
  showExec: readShowExec(),
  issueKind: "", issueState: "open", issueRepo: "", issueQuery: "", issueCursor: 0, issueCursorHistory: [],
  issuePage: null, issueOpener: null,
  lastList: "/runs",
  authGen: 0, // incremented on sign-out and token replacement
  pendingController: null, // AbortController for in-flight API requests
  statusPending: null, statusFailedAt: 0, // the background /api/status fetch (ensureStatus)
};
const runFilters = ["all", "queued", "running", "passed", "failed", "cancelled"];

function readRunFilter() {
  const saved = localStorage.getItem("oberth-filter");
  if (saved === "waiting") return "queued";
  if (saved === "interrupted" || saved === "superseded") return "cancelled";
  return ["queued", "running", "passed", "failed", "cancelled"].includes(saved) ? saved : "all";
}

/* ---------- tiny utilities (unchanged from the old dashboard) ---------- */
function esc(v) { return String(v ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
function enc(v) { return encodeURIComponent(String(v ?? "")); }
function replaceApp(html) { app.innerHTML = html; }
function shortSha(s) { return s ? String(s).slice(0, 7) : "--"; }
function shortId(s) { s = String(s || ""); return s.length > 13 ? s.slice(0, 13) + "…" : (s || "--"); }
function shortSuccessor(s) { return String(s || "").slice(0, 12); }
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
function pageRange(start, length, total) { return length ? `${start}–${start + length - 1} of ${total}` : `0 of ${total}`; }
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
    case "passed": case "delivered": return "pass";
    case "failed": case "timed_out": return "fail";
    case "running": return "run";
    /* "queued" is Argo made a Pod for this and it has not started; "pending"
       is the pipeline declares this and execution has not reached it. Both
       read as not-yet-started to the eye, so they share a kind and a glyph —
       the label keeps them apart wherever the difference matters. */
    case "queued": case "waiting": case "pending": return "pending";
    case "interrupted": case "superseded": case "cancelled": case "skipped": return "cancel";
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
function displayRunStatus(run) {
  if (run.display_status) return run.display_status;
  if (run.Status === "interrupted") return "cancelled";
  if (run.Status === "waiting" || run.Status === "queued" || (run.Status === "running" && !run.StartedAt)) return "queued";
  return run.Status;
}
function runStatusCell(run) {
  const status = displayRunStatus(run);
  return `${statusIcon(status)}${pill(statusLabel(status), statusKind(status))}`;
}
function releaseBadge(run) { return run.Release ? '<span class="v4b release">⏏ RELEASE</span>' : ""; }
function runDuration(run) {
  return spanSeconds(run.StartedAt, run.FinishedAt, run.Status === "running");
}
function runWhen(run) { return run.StartedAt || run.FinishedAt || run.QueuedAt || run.CreatedAt; }
function runMessage(run) {
  if (run.display_reason) return run.display_reason;
  if (run.Error) return run.Error;
  if (run.FailedStep) return `failed at ${run.FailedBurn ? run.FailedBurn + " / " : ""}${run.FailedStep}`;
  if (run.Reason) return run.Reason;
  if (displayRunStatus(run) === "queued") return run.Scheduling?.message || "awaiting scheduler";
  return "";
}
/* Repository names repeat across upstreams (a "terraform" under github,
   codeberg and skipops), and a bare name is then an ambiguous selector the
   API refuses. The dashboard keys a repository by upstream/org/name — the
   qualified selector the API resolves — and labels a shared name org/name. */
function upstreamOf(repo) {
  return (state.status?.upstream_info || []).find(upstream => upstream.id === repo.UpstreamID);
}
function orgOfURL(baseURL) {
  const trimmed = String(baseURL || "").replace(/\/+$/, "");
  return trimmed.slice(trimmed.lastIndexOf("/") + 1);
}
function repoOrg(upstream) {
  return orgOfURL(upstream.base_url) || upstream.name;
}
function repoKey(repo) {
  const upstream = upstreamOf(repo);
  return upstream ? `${upstream.name}/${repoOrg(upstream)}/${repo.Name}` : repo.Name;
}
/* A shared name is shown as org/repo: two upstreams never share an
   organization (the store refuses it), so that is always unique. */
function repoLabel(repo) {
  if (state.repos.filter(other => other.Name === repo.Name).length < 2) return repo.Name;
  const upstream = upstreamOf(repo);
  return upstream ? `${repoOrg(upstream)}/${repo.Name}` : repo.Name;
}
function repoKeyOf(repoID) {
  const repo = state.repos.find(r => r.ID === repoID);
  return repo ? repoKey(repo) : "";
}
function repoName(repoID) {
  const repo = state.repos.find(r => r.ID === repoID);
  return repo ? repoLabel(repo) : (repoID ? "repo#" + repoID : "workspace");
}
/* A filter saved before repositories were keyed (a bare name) maps to the one
   repository of that name, or back to everything when the name is shared.
   Without /api/status the keys cannot be built, so nothing is changed: a
   status blip must not erase a saved filter. */
function normalizeRepoSelector(value, everything) {
  if (!state.status) return value;
  if (!value || value === everything || state.repos.some(repo => repoKey(repo) === value)) return value;
  const named = state.repos.filter(repo => repo.Name === value);
  return named.length === 1 ? repoKey(named[0]) : everything;
}
/* ensureStatus fetches /api/status once in the background. It probes every
   upstream, so pages render without waiting for it and re-render when it
   arrives; a failure is not retried for 30 seconds, so a live view polling
   every two seconds does not hammer it. */
function ensureStatus() {
  if (state.status) return Promise.resolve();
  if (state.statusPending) return state.statusPending;
  if (Date.now() - (state.statusFailedAt || 0) < 30000) return Promise.resolve();
  state.statusPending = loadStatus({ detached: true })
    .catch(err => {
      // A changed token is not a failure of the status endpoint.
      if (err?.name !== "AbortError" && err?.message !== "stale request") state.statusFailedAt = Date.now();
    })
    .finally(() => { state.statusPending = null; });
  return state.statusPending;
}
/* A legacy bare selector needs the keys before it can be resolved. */
function isQualifiedSelector(value) {
  return String(value || "").split("/").length >= 3;
}
/* loadRepoContext loads the repositories and starts /api/status without
   waiting for it: the pending promise is returned in an object, because an
   async function returning a promise would adopt (and wait for) it. */
async function loadRepoContext() {
  if (!state.repos.length) await loadRepos();
  return { pending: ensureStatus() };
}
/* rerenderWhenStatus re-renders a page once /api/status arrives, so names
   shared across upstreams gain their org/ qualifier. */
function rerenderWhenStatus(pending, seq) {
  if (state.status || !pending) return;
  pending.then(() => { if (state.status && currentRoute(seq)) route(true); });
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
  closeLiveLog();
  clearInterval(state.timer);
  replaceApp(`<section class="screen"><div class="table token-panel"><h1>Authentication required</h1><p class="meta">Enter the Oberth uplink bearer token. It is stored in this browser's localStorage so navigation and reloads never re-prompt.</p>${message ? `<p class="meta chain-bad">${esc(message)}</p>` : ""}<input id="tokenInput" type="password" placeholder="Bearer token" autocomplete="off"><div class="token-actions"><button class="chip on" data-action="submit-token">Connect</button></div></div></section>`);
  document.getElementById("tokenInput")?.focus();
}
function submitToken() {
  const value = document.getElementById("tokenInput")?.value;
  if (!value) return;
  closeLiveLog();
  state.authGen++;
  if (state.pendingController) { state.pendingController.abort(); state.pendingController = null; }
  setToken(value);
  route();
}
function clearToken() {
  closeLiveLog();
  state.authGen++;
  if (state.pendingController) { state.pendingController.abort(); state.pendingController = null; }
  setToken(null); setConn(false, "signed out"); showTokenPrompt();
}

/* api fetches for the current route: a response that lands after the route
   moved on is discarded ("stale request"), and navigation aborts it. A
   detached request (opts.detached) belongs to no route — the background
   /api/status fetch — so only a change of token discards it. */
async function api(url, opts = {}) {
  const gen = state.authGen;
  const seq = state.routeSeq;
  const controller = new AbortController();
  if (!opts.detached) state.pendingController = controller;
  let response;
  try {
    response = await fetch(url, { cache: "no-store", headers: authHeaders(), signal: controller.signal });
  } catch (err) {
    if (err.name === "AbortError") throw err;
    setConn(false, "network unreachable");
    throw new Error("Oberth API is unreachable");
  }
  // Discard the response if auth changed or route changed while we waited.
  if (state.authGen !== gen || (!opts.detached && state.routeSeq !== seq)) throw new Error("stale request");
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
  /* opts.withNext also returns the next-page cursor the server names. */
  if (opts.withNext) return { body: await response.json(), next: response.headers.get("X-Oberth-Next-Before") || "" };
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
const runPageSize = 100;
/* The default list has the whole queue first, then admitted and terminal
   runs in descending admission time. */
async function loadRuns() {
  const page = await api(`/api/runs?limit=${runPageSize}`, { withNext: true });
  state.runs = page.body || [];
  state.runsNext = page.next;
}
/* The list pages with ?before (#050). state.runs stays the newest page, for
   the stats; state.repoRuns is the page shown when it is not that one. */
async function loadRepoRuns() {
  if (state.repo === "all" && !state.runCursor) { state.repoRuns = []; state.repoRunsNext = ""; return; }
  const query = new URLSearchParams({ limit: String(runPageSize) });
  if (state.repo !== "all") query.set("repo", state.repo);
  if (state.runCursor) query.set("before", String(state.runCursor));
  try {
    const page = await api("/api/runs?" + query.toString(), { withNext: true });
    state.repoRuns = page.body || [];
    state.repoRunsNext = page.next;
  } catch (err) {
    if (err.status === 404 || err.status === 400 || err.status === 409) { changeRunRepo("all"); localStorage.setItem("oberth-repo", "all"); state.repoRuns = []; state.repoRunsNext = ""; return; }
    throw err;
  }
}
/* Older servers have no counts route; the sidebar then counts the page. */
async function loadRunCounts() {
  try { state.runCounts = await api("/api/runs/counts"); } catch (err) {
    if (err.message === "stale request" || err.name === "AbortError") throw err;
    state.runCounts = null;
  }
}
function changeRunRepo(repo) {
  state.repo = repo;
  state.runCursor = 0;
  state.runCursorHistory = [];
  state.runStart = 1;
  state.runStartHistory = [];
}
async function loadStatus(opts) {
  state.status = await api("/api/status", opts);
  setVersion(true, state.status?.version);
}

/* ---------- runs list ---------- */
/* A repository filter is applied by the server (loadRepoRuns); status and
   release kind compose on the loaded page. */
function runMatches(run) {
  const statusOK = state.filter === "all" || displayRunStatus(run) === state.filter;
  return statusOK && (!state.releaseOnly || run.Release);
}
function repoCounts(runs) {
  const counts = new Map([["all", runs.length]]);
  for (const run of runs) {
    const key = repoKeyOf(run.RepoID);
    counts.set(key, (counts.get(key) || 0) + 1);
  }
  return counts;
}
/* ---------- one run list: the queue, then admission time ---------- */
function admissionOf(run) { return run.Scheduling?.admission || null; }
function queuePhase(run) {
  const status = displayRunStatus(run);
  return status === "queued" && run.Scheduling?.state === "waiting" ? "waiting" : status;
}
function queueSize(run) { return admissionOf(run)?.size || ""; }
function queueSizeTitle(run) {
  const admission = admissionOf(run);
  if (!admission?.size) return "";
  return `declared size ${admission.size}${admission.capacity > 0 ? ` · admission weight ${admission.weight} of ${admission.capacity}` : ""}`;
}
function queueReason(run) {
  const a = admissionOf(run), s = run.Scheduling?.state, b = a?.blocked_by;
  if (s === "preflight") return "KVM preflight running";
  if (!a || s === "preparing") return "preparing to run";
  if (a.kubevirt_capacity) return "waiting for KubeVirt capacity (repository VM grant)";
  if (a.vm_capacity) return "waiting for trusted VM capacity";
  if (a.disk_floor?.below_floor) return "waiting for disk space";
  if (a.group_holder || a.group_ahead) return `waiting for group ${a.group}`;
  if (a.head_of_queue && b) return `waiting for capacity (${b.size}${b.full_drain ? ", needs the whole host" : ""})`
    + (b.backfilling ? " · smaller runs admitted meanwhile" : b.draining ? " · host draining for it" : "");
  if (b) return `waiting behind ${b.repo}/${b.ref} (${b.size})`;
  if (s === "waiting" && a.state === "waiting") return `waiting for capacity${a.size ? ` (${a.size})` : ""}`;
  if (s === "waiting") return "admitted, submitting workflow";
  return run.Scheduling?.message ? "awaiting scheduler" : runMessage(run) || "awaiting scheduler";
}
function queueDetail(run) { return run.Scheduling?.message || runMessage(run); }
function runTimeLabel(run) {
  const status = displayRunStatus(run);
  if (status === "queued") return `queued ${ago(run.QueuedAt)}`;
  if (run.StartedAt) return `started ${ago(run.StartedAt)}`;
  return `ended ${ago(run.FinishedAt || run.QueuedAt)}`;
}
function runMessageCell(run) {
  const message = displayRunStatus(run) === "queued" ? queueReason(run) : runMessage(run);
  if (run.SupersededBy) {
    return `<button type="button" class="run-successor" data-run-id="${esc(run.SupersededBy)}">${esc(message)}</button>`;
  }
  return run.display_reason === "server restart, retry needed" ? `<span class="retry-warning">${esc(message)}</span>` : esc(message);
}
function runsTable(runs) {
  return `<div class="table runs-table"><div class="trow head"><div>status</div><div>run</div><div>sha</div><div>actor</div><div>when</div><div>message</div></div>${runs.map(run => {
    const queued = displayRunStatus(run) === "queued";
    const status = queuePhase(run);
    const group = admissionOf(run)?.group || run.ConcurrencyGroup;
    return `<div class="trow" data-run-id="${esc(run.ID)}"><div class="status-cell run-status">${statusIcon(status)}${pill(statusLabel(status), statusKind(status))}</div><div class="primary"><div class="repo-name">${esc(repoName(run.RepoID))} ${releaseBadge(run)}${group ? ` <span class="v4b dim" title="concurrency group">${esc(group)}</span>` : ""}</div><div class="branch">${esc(run.RefKind === "tag" ? "tag " : "")}${esc(run.Ref)}</div></div><div class="sha">${esc(shortSha(run.SHA))}</div><div class="clip" title="${esc(run.Actor)}">${esc(run.Actor || "--")}</div><div class="nowrap" title="${esc(fmtTime(runWhen(run)))}">${esc(runTimeLabel(run))}<br><span class="meta"${queued ? ` title="${esc(queueSizeTitle(run))}"` : ""}>${queued ? esc(queueSize(run)) : esc(fmtDur(runDuration(run)))}</span></div><div class="clip" title="${esc(queueDetail(run))}">${runMessageCell(run)}</div></div>`;
  }).join("")}</div>`;
}
async function renderRuns(seq) {
  setChrome("runs", "/ runs");
  await loadRepos();
  const pending = ensureStatus();
  if (state.repo !== "all" && !isQualifiedSelector(state.repo)) await pending;
  state.repo = normalizeRepoSelector(state.repo, "all");
  await Promise.all([loadRuns(), loadRepoRuns(), loadRunCounts()]);
  if (!currentRoute(seq)) return;
  rerenderWhenStatus(pending, seq);
  localStorage.setItem("oberth-filter", state.filter);
  localStorage.setItem("oberth-kind", state.releaseOnly ? "release" : "all");
  localStorage.setItem("oberth-repo", state.repo);
  const runs = state.runs, source = state.repo !== "all" || state.runCursor ? state.repoRuns : runs;
  const visible = source.filter(runMatches), counts = repoCounts(runs);
  if (state.runCounts) {
    counts.set("all", state.runCounts.Total || 0);
    for (const repo of state.repos) counts.set(repoKey(repo), state.runCounts.ByRepo?.[repo.ID] || 0);
  } else if (state.repo !== "all" && !state.runCursor) counts.set(state.repo, state.repoRuns.length);
  /* The server names the older page's cursor: a run sequence, or a run ID
     for an uplink scoped to some upstreams, which sees no sequences (#073).
     A server without the header pages by the oldest run's sequence. */
  const named = source === runs ? state.runsNext : state.repoRunsNext;
  const dated = source.filter(run => displayRunStatus(run) !== "queued");
  state.runSourceCount = dated.length;
  state.runNext = dated.length === runPageSize ? (named || source[source.length - 1].QueueSequence || 0) : 0;
  const page = state.runCursorHistory.length + 1;
  const selectedRepo = state.repos.find(repo => repoKey(repo) === state.repo);
  const current = runs.filter(run => state.repo === "all" || repoKeyOf(run.RepoID) === state.repo);
  const queued = (state.repo === "all" ? state.runCounts?.Queued : state.runCounts?.QueuedByRepo?.[selectedRepo?.ID]) ?? current.filter(run => displayRunStatus(run) === "queued").length;
  const datedTotal = Math.max(0, (counts.get(state.repo) || 0) - queued);
  replaceApp(`
  <section class="screen">
    <h1 class="sr-only">Runs</h1>
    <div class="runs-layout">
      <aside class="repo-side">
        <div class="repo-filter-row"><button class="repo-filter ${state.repo === "all" ? "on" : ""}" data-repo-filter="all"><span class="name">All repos</span><span class="count">${esc(counts.get("all") || 0)}</span></button></div>
        ${orderedRepos().map((repo, index, repos) => {
          const divider = repo.ArchivedAt && (index === 0 || !repos[index - 1].ArchivedAt) ? '<div class="repo-filter-row repo-archived-divider"><span class="meta">archived</span></div>' : "";
          return `${divider}<div class="repo-filter-row"><button class="${repo.ArchivedAt ? "repo-filter archived" : "repo-filter"}${state.repo === repoKey(repo) ? " on" : ""}" data-repo-filter="${esc(repoKey(repo))}" title="${esc(repoKey(repo))}"><span class="name">${esc(repoLabel(repo))}</span><span class="count">${esc(counts.get(repoKey(repo)) || 0)}</span></button></div>`;
        }).join("")}
      </aside>
      <section>
        <div class="toolbar">
          ${runFilters.map(f => `<button class="chip ${state.filter === f ? "on" : ""}" data-status-filter="${esc(f)}">${esc(f)}</button>`).join("")}
          <button class="chip ${state.releaseOnly ? "on" : ""}" data-kind-filter="release" aria-pressed="${state.releaseOnly}">release</button>
          <span class="count">${pageRange(state.runStart, dated.length, datedTotal)}${state.filter === "all" && !state.releaseOnly ? "" : ` · ${visible.length} match this page`}</span>
        </div>
        ${visible.length ? runsTable(visible) : `<div class="empty">No runs match the current filters.</div>`}
        ${page > 1 || state.runNext ? `<div class="issue-pager">
          <button type="button" data-action="runs-newer" ${page > 1 ? "" : "disabled"}>‹ newer</button>
          <button type="button" data-action="runs-older" ${state.runNext ? "" : "disabled"}>older ›</button>
        </div>` : ""}
      </section>
    </div>
  </section>`);
  setAuto(listPollMs);
}

/* ---------- run detail: burn/step breakdown + retained logs ---------- */
function stepKey(step) { return step.Burn + " / " + step.Step; }
/* Argo names the attempts of a retried template task(N). The server folds
   such a row onto its planned declaration, but an attempt of a step the plan
   never declared keeps the suffix on its burn (or, for a retried Pod, its
   step). Read it back here so the row renders as the same logical step with a
   retry marker -- and only when its first attempt is in the list, so a name
   that merely ends in (N) is left alone. */
function stepIdentity(step, keys) {
  const strip = name => { const match = /^(.*[^\s(])\((\d+)\)$/.exec(name || ""); return match ? { name: match[1], attempt: Number(match[2]) } : { name: name || "", attempt: 0 }; };
  const burn = strip(step.Burn), name = strip(step.Step);
  const attempt = burn.attempt || name.attempt;
  if (attempt && keys.has(`${burn.name}/${name.name}`)) return { burn: burn.name, step: name.name, attempt };
  return { burn: step.Burn || "", step: step.Step || "", attempt: 0 };
}
function stepLabel(identity) { return identity.burn === identity.step ? identity.step : `${identity.burn}/${identity.step}`; }
/* Keep each burn contiguous, ordered by its earliest start, then plan order.
   Within a burn, started steps precede unstarted steps and retries stay with
   their logical burn. Finishing a step cannot change the order. Ordinals are
   the plan's order, never the timing observer's order. */
function orderSteps(steps) {
  const keys = new Set((steps || []).map(step => `${step.Burn}/${step.Step}`));
  const burns = new Map();
  const rows = (steps || []).map((step, index) => {
    const identity = stepIdentity(step, keys);
    const parsed = Date.parse(step.StartedAt || "");
    const started = Number.isFinite(parsed) ? parsed : Infinity;
    if (!burns.has(identity.burn)) burns.set(identity.burn, { burnStart: Infinity, burnOrdinal: Infinity, index });
    const burn = burns.get(identity.burn);
    burn.burnStart = Math.min(burn.burnStart, started);
    burn.burnOrdinal = Math.min(burn.burnOrdinal, step.Ordinal);
    return { step, index, identity, label: stepLabel(identity), key: `${identity.burn}/${identity.step}`, started, burn };
  });
  rows.sort((a, b) => {
    if (a.burn !== b.burn) {
      if (a.burn.burnStart !== b.burn.burnStart) return a.burn.burnStart - b.burn.burnStart;
      return a.burn.burnOrdinal - b.burn.burnOrdinal || a.burn.index - b.burn.index;
    }
    if (a.started !== b.started) return a.started - b.started;
    return a.identity.attempt - b.identity.attempt || a.step.Ordinal - b.step.Ordinal || a.index - b.index;
  });
  return rows;
}
/* Every step is the same row: icon, name, duration. The only variation is
   the status icon, the duration column (a state word until the step starts),
   and the small retry / secret markers. */
function stepRow(row, selectedKey) {
  const { step, label, identity } = row;
  const key = stepKey(step), sk = statusKind(step.Status);
  const seconds = spanSeconds(step.StartedAt, step.FinishedAt, sk === "run");
  // A row that ran shows its duration, never "--": a sub-second step is "<1s".
  const timed = step.StartedAt && (step.FinishedAt || sk === "run");
  const column = timed ? (seconds < 1 ? "<1s" : fmtDur(seconds)) : sk === "cancel" ? "skipped" : statusLabel(step.Status);
  return `<button class="v4row${key === selectedKey ? " sel" : ""}${sk === "pending" ? " pend" : ""}" data-select-step="${esc(key)}"><span class="ic ${sk === "pass" ? "g" : sk === "fail" ? "r" : sk === "run" ? "o" : "d"}">${sk === "pass" ? "✓" : sk === "fail" ? "✗" : sk === "run" ? "◠" : sk === "cancel" ? "⊘" : "◌"}</span><span class="nm">${esc(label)}</span>${identity.attempt ? `<span class="tag">retry ${esc(identity.attempt)}</span>` : ""}${step.Secrets ? '<span class="tag">secret</span>' : ""}<span class="dur">${esc(column)}</span></button>`;
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
  execNoise: "^time=\\S+ level=(info|debug) (.*\\s)?argo=true(\\s|$)",
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
/* Argo's executor (argoexec emissary) runs as PID 1 of every step's main
   container and logs its own lifecycle into the step's stream: "waiting for
   signals", "sub-process exited" error=<nil>. Those INFO lines are folded
   behind a toggle. A WARN/ERROR line or a non-nil error — the exit status of
   a failed step — is never folded (logLineClass judges it), and the stored
   log and the API keep every line. runlog.IsExecutorNoise is the Go twin. */
function isExecutorNoise(line) {
  const body = String(line || "").replace(LOG_RE.stepPrefix, "").trim();
  if (!LOG_RE.execNoise.test(body) || logLineClass(line) !== "") return false;
  for (const field of body.matchAll(LOG_ERR_FIELD_ALL)) {
    if (!LOG_NIL_VALUES.has(String(field[3] || "").toLowerCase())) return false;
  }
  return true;
}
function readShowExec() {
  try { return localStorage.getItem("oberth-show-exec") === "1"; } catch { return false; }
}
function toggleExecutorLines() {
  state.showExec = !state.showExec;
  try { localStorage.setItem("oberth-show-exec", state.showExec ? "1" : "0"); } catch { /* preference only */ }
  document.getElementById("termBody")?.classList.toggle("show-exec", state.showExec);
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
const LOG_STEP_MARK = /^\[([^\]/]+)\/([^\]]+)\] /;
/* Attribution belongs to newline-delimited retained records. CR progress
   updates within one record all belong to that record's step, even when the
   command text happens to contain another bracketed step name. */
function splitAttributedLogText(raw) {
  const records = String(raw || "").split("\n");
  if (records[records.length - 1] === "") records.pop();
  return records.flatMap(record => {
    const prefix = LOG_STEP_MARK.exec(record)?.[0] || "";
    const body = record.slice(prefix.length).replace(/\r$/, "");
    return splitLogText(body + "\n").map(line => prefix + line);
  });
}
/* renderLogLines numbers lines by their position in the log, so a number
   still matches oberth log and the API. With groupSteps (the combined live
   view) the repeated [burn/step] prefix becomes one header row per step. */
function renderLogLines(lines, opts = {}) {
  const total = lines.length;
  const truncated = total > maxRenderedLogLines;
  const start = truncated ? total - maxRenderedLogLines : 0;
  let out = "", current = opts.carry?.current || "", noise = [];
  const base = opts.carry?.count || 0;
  const recordAttr = opts.carry ? ' data-log-record=""' : "";
  const flushNoise = () => {
    if (!noise.length) return;
    const n = noise.length;
    out += `<button type="button" class="log-line log-fold" data-action="toggle-exec" title="Argo executor lifecycle lines; warnings, errors and exit statuses are never hidden"><span class="log-no"></span><span class="log-txt"><span class="fold-show">▸ ${n} Argo executor line${n === 1 ? "" : "s"}</span><span class="fold-hide">▾ hide Argo executor lines</span></span></button>` + noise.join("");
    noise = [];
  };
  if (truncated && !opts.carry) {
    out += `<div class="log-line"><span class="log-no">--</span><span class="log-txt warn">[truncated] showing last ${maxRenderedLogLines} of ${total} lines</span></div>`;
  }
  for (let i = start; i < total; i++) {
    let line = lines[i];
    if (line && typeof line === "object" && line.successor) {
      flushNoise();
      out += `<div class="log-line"${recordAttr}><span class="log-no">${base + i + 1}</span><span class="log-txt"><button type="button" class="run-successor" data-run-id="${esc(line.successor)}">${esc(line.text)}</button></span></div>`;
      continue;
    }
    if (opts.groupSteps) {
      const mark = LOG_STEP_MARK.exec(line);
      if (mark) {
        const key = mark[1] + " / " + mark[2];
        if (key !== current) {
          flushNoise();
          current = key;
          out += `<div class="log-line log-hdr"><span class="log-no"></span><span class="log-txt">── ${esc(key)} ──</span></div>`;
        }
        line = line.slice(mark[0].length);
      }
    }
    if (isExecutorNoise(line)) {
      noise.push(`<div class="log-line exec"${recordAttr}><span class="log-no">${base + i + 1}</span><span class="log-txt">${esc(line)}</span></div>`);
      continue;
    }
    flushNoise();
    out += `<div class="log-line"${recordAttr}><span class="log-no">${base + i + 1}</span><span class="log-txt ${logLineClass(line)}">${esc(line) || "&nbsp;"}</span></div>`;
  }
  flushNoise();
  if (opts.carry) { opts.carry.count = base + total; opts.carry.current = current; }
  return out;
}
function summaryLines(detail) {
  const run = detail.Run || {}, steps = detail.Steps || [];
  const lines = [
    `$ oberth run ${repoName(run.RepoID)}/${run.Ref}`,
    `run ${run.ID} status=${statusLabel(displayRunStatus(run))} sha=${shortSha(run.SHA)} trigger=${run.Trigger || "--"}`,
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
  const status = displayRunStatus(run), kind = statusKind(status);
  if (kind === "pass") lines.push(`[pass] ${passed} step${passed === 1 ? "" : "s"} passed across ${burns} burn${burns === 1 ? "" : "s"}`);
  else if (kind === "fail") lines.push(`[fail] failed at ${run.FailedBurn ? run.FailedBurn + " / " : ""}${run.FailedStep || "unknown step"} — ${passed} step${passed === 1 ? "" : "s"} passed`);
  else if (kind === "run") lines.push(`[running] running in phase ${run.Phase || "ci"} — ${reached} of ${steps.length} step${steps.length === 1 ? "" : "s"} started, ${passed} passed`);
  else if (status === "queued") {
    const scheduling = detail.Scheduling || {}, admission = scheduling.admission || {};
    lines.push(`[queued] ${scheduling.message || "Awaiting scheduler."}`);
    if (admission.weight) lines.push(`capacity: need ${admission.weight} weight · ${admission.used}/${admission.capacity} used${admission.position ? ` · queue position ${admission.position}` : ""}`);
  }
  else if (kind === "cancel") lines.push(`[${status}] ${runMessage(run)}`);
  else lines.push(`[queued] queued behind earlier work for this repository`);
  if (unreached) lines.push(`${unreached} declared step${unreached === 1 ? "" : "s"} not reached`);
  if (run.SupersededBy) lines.push({successor: run.SupersededBy, text: run.display_reason || `view successor ${shortSuccessor(run.SupersededBy)}`});
  if (run.Error) lines.push("", `error: ${run.Error}`);
  const timed = steps.map(step => ({ step, seconds: spanSeconds(step.StartedAt, step.FinishedAt, false) })).filter(item => item.seconds > 0).sort((a, b) => b.seconds - a.seconds);
  if (timed.length) lines.push("", `slowest: ${timed.slice(0, 2).map(item => `${stepKey(item.step)} ${fmtDur(item.seconds)}`).join(" · ")}`);
  lines.push("", "select a step for its retained log");
  return lines;
}
async function stepLogContent(runID, step, active) {
  if (step.Status === "pending") return renderLogLines([`step: ${stepKey(step)} · pending`, "", "step is declared by the pipeline and has not been reached yet"]);
  if (statusKind(step.Status) === "pending") return renderLogLines([`step: ${stepKey(step)} · queued`, "", "step is waiting for its turn"]);
  const cacheKey = `${runID}|${step.Burn}|${step.Step}|${step.FinishedAt || ""}`;
  if (!active && state.logCache.has(cacheKey)) return state.logCache.get(cacheKey);
  let html;
  try {
    const payload = await api(`/api/runs/${enc(runID)}/logs?burn=${enc(step.Burn)}&step=${enc(step.Step)}`);
    const lines = stripStepPrefix(splitAttributedLogText(payload.output), step.Burn, step.Step);
    html = renderLogLines([`step: ${stepKey(step)} · ${statusLabel(step.Status)}${step.ExitCode ? ` · exit ${step.ExitCode}` : ""}`, ""].concat(lines.length ? lines : ["no retained step output"]));
  } catch (err) {
    html = renderLogLines([`step: ${stepKey(step)} · ${statusLabel(step.Status)}`, "", `step log unavailable: ${err.message || err}`]);
  }
  if (state.logCache.size > 32) state.logCache.clear();
  if (!active) state.logCache.set(cacheKey, html);
  return html;
}
/* Polling health and pod delivery health are separate observations. Empty
   successful reads are healthy polling; they say nothing about command work. */
const deliveryPrefix = "::oberth-log:: ";
function selectedLiveLines(lines, burn, step) {
  const prefix = "[" + burn + "/" + step + "] ";
  return lines.filter(line => line.startsWith(prefix)).map(line => line.slice(prefix.length));
}
function ingestLiveChunk(live, chunk, now, done = false) {
  // Keep the head of an oversized record, including its attribution, and
  // discard its continuation until the real newline. A suffix is not a new
  // record and must never be allowed to supply a different step prefix.
  if (live.pendingTruncated) {
    const newline = chunk.indexOf("\n");
    if (newline < 0 && !done) return [];
    chunk = newline < 0 ? "" : chunk.slice(newline);
  }
  const output = [];
  const records = (live.pending + chunk).split("\n");
  live.pending = records.pop();
  if (records.length) live.pendingTruncated = false;
  if (live.pending.length > 400000) {
    live.pending = live.pending.slice(0, 300000) + " … [truncated]";
    live.pendingTruncated = true;
  }
  // Once the server confirms EOF, a final line needs no trailing newline.
  if (done && live.pending) { records.push(live.pending); live.pending = ""; live.pendingTruncated = false; }
  for (const record of records) {
    const line = record.length > 400000 ? record.slice(0, 300000) + " … [truncated]" : record;
    if (line.startsWith(deliveryPrefix)) {
      try {
        const delivery = JSON.parse(line.slice(deliveryPrefix.length));
        live.delivery.set(delivery.burn + "/" + delivery.step + "/" + (delivery.node || ""), delivery);
      } catch { /* malformed metadata never becomes command output */ }
      continue;
    }
    const mark = LOG_STEP_MARK.exec(line);
    if (mark) live.lastOutput.set(mark[1] + "/" + mark[2], now);
    live.lastAnyOutput = now;
    live.text += line + "\n";
    output.push(line);
  }
  if (live.text.length > 400000) {
    const cut = live.text.indexOf("\n", live.text.length - 300000);
    live.text = cut < 0 ? "" : live.text.slice(cut + 1);
  }
  return output;
}
function liveDeliverySummary(live, burn, step, now = Date.now()) {
  const key = burn && step ? burn + "/" + step : "";
  const last = key ? live.lastOutput.get(key) : live.lastAnyOutput;
  const age = Math.max(0, (now - (last || live.startedAt)) / 1000);
  const poll = live.error ? `reconnecting: ${live.error} · last successful poll ${live.lastPoll ? fmtDur((now - live.lastPoll) / 1000) + " ago" : "not yet"}` : live.connected && !live.fallback ? "stream connected" : live.lastPoll ? "polling connected" : "connecting";
  const deliveries = [...live.delivery.values()].filter(d => !key || (d.burn === burn && d.step === step));
  const deliveryStale = d => !d.at || ((now - Date.parse(d.at)) > 10000 && !["complete", "gapped", "truncated", "unavailable"].includes(d.state));
  const unhealthy = deliveries.filter(d => deliveryStale(d) || d.gap || ["reconnecting", "unavailable", "gap", "gapped", "truncated"].includes(d.state));
  const delivery = unhealthy.length ? unhealthy.map(d => `${d.burn}/${d.step}: ${deliveryStale(d) ? "delivery unconfirmed · last observed " : ""}${d.gap ? "continuity gap · " : ""}${d.state}${d.message ? " — " + d.message : ""}`).join("; ") : deliveries.length ? `pod delivery: ${[...new Set(deliveries.map(d => d.state))].join(", ")}` : "pod delivery not yet observed";
  return `${poll} · ${last ? "last output received " + fmtDur(age) + " ago" : "no output observed for " + fmtDur(age)}${age >= 30 ? " · quiet period" : ""} · ${delivery}`;
}
/* EventSource framing over fetch preserves the dashboard's bearer header.
   Native EventSource cannot attach Authorization; tokens never go in URLs. */
class LiveLogEventSource {
  constructor(url, onChunk, onError, onOpen) {
    this.controller = new AbortController();
    this.closed = false;
    this.read(url, onChunk, onError, onOpen);
  }
  close() { this.closed = true; this.controller.abort(); }
  async read(url, onChunk, onError, onOpen) {
    let reader;
    try {
      const response = await fetch(url, {cache: "no-store", headers: {...authHeaders(), Accept: "text/event-stream"}, signal: this.controller.signal});
      if (!response.ok || !response.headers.get("content-type")?.startsWith("text/event-stream") || !response.body) throw new Error("live stream unavailable");
      if (this.closed) return;
      onOpen();
      reader = response.body.getReader();
      const decoder = new TextDecoder();
      let pending = "", event = "", data = [];
      while (!this.closed) {
        const {value, done} = await reader.read();
        if (done) throw new Error("live stream ended");
        pending += decoder.decode(value, {stream: true});
        let newline;
        while ((newline = pending.indexOf("\n")) >= 0 && !this.closed) {
          const line = pending.slice(0, newline).replace(/\r$/, "");
          pending = pending.slice(newline + 1);
          if (!line) {
            if (event === "chunk" && data.length) onChunk(JSON.parse(data.join("\n")));
            event = ""; data = [];
          } else if (line.startsWith("event:")) event = line.slice(6).trim();
          else if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
        }
      }
    } catch (err) {
      if (!this.closed) onError(err);
    } finally {
      if (reader) { await reader.cancel().catch(() => {}); reader.releaseLock(); }
    }
  }
}
function stopLiveTransport(live) {
  live.source?.close(); live.source = null;
  clearInterval(live.pollTimer); live.pollTimer = null;
}
function closeLiveLog() {
  const live = state.live;
  if (!live) return;
  live.closed = true;
  stopLiveTransport(live);
  if (live.frame != null) cancelAnimationFrame(live.frame);
  state.live = null;
}
function liveViewLines(lines, view) {
  return view.burn && view.step ? selectedLiveLines(lines, view.burn, view.step) : lines;
}
function receiveLiveChunk(live, payload) {
  if (state.live !== live || live.closed) return;
  const now = Date.now();
  const records = ingestLiveChunk(live, payload.chunk || "", now, payload.done === true);
  if (typeof payload.offset === "number") live.offset = payload.offset;
  live.lastPoll = now; live.error = ""; live.done = payload.done === true;
  if (live.view) {
    live.view.pending = live.view.pending.concat(liveViewLines(splitAttributedLogText(records.length ? records.join("\n") + "\n" : ""), live.view));
    // Bound queued work even when animation frames are paused in a hidden tab.
    const excess = live.view.pending.length - maxRenderedLogLines;
    if (excess > 0) {
      live.view.pending.splice(0, excess);
      live.view.carry.count += excess;
      live.view.carry.current = "";
    }
  }
  if (live.done) stopLiveTransport(live);
  scheduleLiveFrame(live);
}
async function pollLiveLog(live) {
  if (live.busy || live.done || live.closed || state.live !== live) return;
  live.busy = true;
  try {
    const payload = await api(`/api/runs/${enc(live.runID)}/livelog?offset=${live.offset}`, {detached: true});
    receiveLiveChunk(live, payload);
  } catch (err) {
    if (state.live === live) { live.error = String(err.message || err); scheduleLiveFrame(live); }
  } finally { live.busy = false; }
}
function fallbackLiveLog(live, err) {
  if (state.live !== live || live.closed || live.done) return;
  stopLiveTransport(live);
  live.fallback = true;
  live.error = String(err?.message || "");
  live.pollTimer = setInterval(() => { pollLiveLog(live); }, livePollMs);
  return pollLiveLog(live);
}
function scheduleLiveFrame(live) {
  if (live.frame != null || !live.view?.body || live.closed) return;
  live.frame = requestAnimationFrame(() => {
    live.frame = null;
    if (state.live === live && !live.closed) appendLiveLines(live);
  });
}
function appendLiveLines(live) {
  const view = live.view, body = view?.body;
  if (!body?.isConnected) return;
  const stick = body.scrollHeight - body.scrollTop - body.clientHeight < 40;
  const previousTop = body.scrollTop;
  const lines = view.pending.splice(0);
  if (lines.length) {
    body.querySelector(".log-waiting")?.remove();
    body.insertAdjacentHTML("beforeend", renderLogLines(lines, {groupSteps: !view.burn, carry: view.carry}));
    // Remove old rows in place. Header/fold rows preceding the first retained
    // record are kept only when they still describe that record.
    const records = body.querySelectorAll("[data-log-record]");
    if (records.length > maxRenderedLogLines) {
      const first = records[records.length - maxRenderedLogLines];
      let keep = first;
      while (keep.previousElementSibling?.matches(".log-hdr, .log-fold")) keep = keep.previousElementSibling;
      const oldHeight = body.scrollHeight;
      // If the cap cuts through a step, retain its header for attribution.
      if (!keep.matches(".log-hdr")) {
        let header = keep.previousElementSibling;
        while (header && !header.matches(".log-hdr")) header = header.previousElementSibling;
        if (header) { body.insertBefore(header, keep); keep = header; }
      }
      while (body.firstElementChild !== keep) body.firstElementChild.remove();
      if (!stick) body.scrollTop = Math.max(0, previousTop - (oldHeight - body.scrollHeight));
    }
    if (stick) body.scrollTop = body.scrollHeight;
  }
  const delivery = document.getElementById("liveDelivery");
  if (delivery) delivery.textContent = liveDeliverySummary(live, view.burn, view.step);
  const indicator = document.querySelector(".term-live");
  if (indicator) indicator.textContent = live.error ? "RECONNECTING" : live.done ? "COMPLETE" : live.fallback ? "POLLING" : "LIVE";
}
async function liveLogContent(run, activeBurn, activeStep) {
  if (!state.live || state.live.runID !== run.ID) {
    closeLiveLog();
    const live = state.live = {runID: run.ID, routePath: location.pathname, offset: -1, text: "", pending: "", pendingTruncated: false, busy: false, startedAt: Date.now(), lastPoll: 0, lastOutput: new Map(), delivery: new Map(), error: "", done: false, frame: null};
    if (typeof ReadableStream !== "undefined" && typeof TextDecoder !== "undefined") {
      live.source = new LiveLogEventSource(`/api/runs/${enc(run.ID)}/livelog/stream?offset=${live.offset}`,
        payload => receiveLiveChunk(live, payload), err => fallbackLiveLog(live, err),
        () => { if (state.live === live) { live.connected = true; scheduleLiveFrame(live); } });
    } else await fallbackLiveLog(live);
    if (state.live !== live) return "";
  }
  const live = state.live;
  const key = JSON.stringify([activeBurn || "", activeStep || ""]);
  if (live.view?.key === key && live.view.body?.isConnected) {
    scheduleLiveFrame(live);
    return "";
  }
  const view = live.view = {key, burn: activeBurn, step: activeStep, carry: {count: 0, current: ""}, pending: []};
  const lines = liveViewLines(splitAttributedLogText(live.text), view);
  return renderLogLines(lines, {groupSteps: !activeBurn, carry: view.carry}) || '<div class="log-waiting">waiting for step output…</div>';
}
async function renderRunDetail(runID, seq, background) {
  if (!background) setChrome("runs", "/ run");
  let pending = null;
  try { ({ pending } = await loadRepoContext()); } catch { /* repo names fall back to IDs */ }
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
  rerenderWhenStatus(pending, seq);
}
async function renderRunDetailView(detail, seq) {
  const run = detail.Run || {}, steps = detail.Steps || [];
  const repository = state.repos.some(repo => repo.ID === run.RepoID) ? repoName(run.RepoID) : (detail.Repository?.Name || repoName(run.RepoID));
  if (state.stepRunID !== run.ID) { state.step = ""; state.stepRunID = run.ID; }
  const keys = steps.map(stepKey);
  if (state.step && state.step !== "__summary__" && !keys.includes(state.step)) state.step = "";
  const selectedKey = state.step === "__summary__" ? "" : state.step || defaultStep(detail);
  const selected = steps.find(step => stepKey(step) === selectedKey) || null;
  const status = displayRunStatus(run), kind = statusKind(status);
  const phaseLabel = statusLabel(status).toUpperCase();
  const seconds = runDuration(run);
  const stepCount = steps.length;
  // A running run streams live output in the terminal: on the summary view and
  // on any still-running step. Completed steps keep their retained slice.
  const live = (kind === "run" && (!selected || ["running", "queued"].includes(selected.Status))) || state.step === "__summary__";
  const terminal = live ? await liveLogContent(run, selected?.Burn, selected?.Step) : selected ? await stepLogContent(run.ID, selected, kind === "run") : renderLogLines(summaryLines(detail));
  if (!currentRoute(seq) || (live && !state.live)) return;
  // Preserve the reader's place across the poll re-render: stick to the
  // bottom while they are at the bottom, hold their position when they have
  // scrolled up to read.
  const previousTerm = document.getElementById("termBody");
  const stick = !previousTerm || previousTerm.scrollHeight - previousTerm.scrollTop - previousTerm.clientHeight < 40;
  const previousScroll = previousTerm ? previousTerm.scrollTop : 0;
  const previousStepList = document.getElementById("stepList");
  const previousStepScroll = previousStepList ? previousStepList.scrollTop : 0;
  setChrome("runs", `/ ${repository}`);
  const detailHTML = `
  <section class="screen wide" data-detail-run="${esc(run.ID)}">
    <div class="rh">
      <div class="rh1">
        ${bigIcon(status)}
        <span class="rh-name"><span class="rp">${esc(repository)} /</span> ${esc(run.Ref)}</span>
        <span class="v4b ${kind === "pass" ? "pass" : kind === "fail" ? "fail" : kind === "run" ? "run" : "dim"}">${esc(phaseLabel)}</span>
        ${releaseBadge(run)}
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
    <div id="runBanners">${kind === "fail" ? `<div class="fbanner"><b>✗ ${esc(run.FailedStep ? (run.FailedBurn ? run.FailedBurn + " / " : "") + run.FailedStep : "run failed")}</b><span class="err" title="${esc(run.Error)}">${esc(run.Error || "")}</span></div>` : ""}
    ${status === "cancelled" ? `<div class="sbanner${runMessage(run) === "server restart, retry needed" ? " retry-warning" : ""}"><span class="err">${esc(runMessage(run))}</span>${run.SupersededBy ? `<button class="chip" data-run-id="${esc(run.SupersededBy)}">view successor ${esc(shortSuccessor(run.SupersededBy))}</button>` : ""}</div>` : ""}
    </div><div class="v4-body">
      <div class="slist" id="stepList">
        <div class="sl-h"><span>STEPS · ${stepCount}${run.Release ? " · RELEASE" : ""}</span></div>
        <button class="v4row summary${selected ? "" : " sel"}" data-select-step=""><span class="ic ${kind === "pass" ? "g" : kind === "fail" ? "r" : kind === "run" ? "o" : "d"}">≡</span><span class="nm">Run summary</span><span class="dur">${esc(fmtDur(seconds))}</span></button>
        <div class="sl-sep"></div>
        ${orderSteps(steps).map(row => stepRow(row, selected ? selectedKey : "")).join("") || `<div class="empty">${run.Phase === "preflight" ? "Trusted KVM preflight running — steps appear after submission." : "No step results recorded yet."}</div>`}
      </div>
      <div class="logp">
        <div class="log-h" id="logHead">${selected ? `${statusIcon(selected.Status)}<span class="nm">${esc(stepKey(selected))}</span><span class="v4b ${statusKind(selected.Status) === "pass" ? "pass" : statusKind(selected.Status) === "fail" ? "fail" : statusKind(selected.Status) === "run" ? "run" : "dim"}">${esc(statusLabel(selected.Status).toUpperCase())}</span>` : `<span class="ic ${kind === "pass" ? "g" : kind === "fail" ? "r" : kind === "run" ? "o" : "d"}">≡</span><span class="nm">Run summary</span><span class="v4b ${kind === "pass" ? "pass" : kind === "fail" ? "fail" : kind === "run" ? "run" : "dim"}">${esc(phaseLabel)} · ${esc(fmtDur(seconds))}</span>`}</div>
        <div class="terminal"><div class="term-bar"><span class="lamp red"></span><span class="lamp yellow"></span><span class="lamp green"></span><span class="term-title">${esc(repository)} / ${esc(run.Ref)} — ${live ? "live output" : selected ? esc(stepKey(selected)) : "summary"}</span>${live ? `<span class="term-live">${state.live?.error ? "RECONNECTING" : state.live?.done ? "COMPLETE" : state.live?.fallback ? "POLLING" : "LIVE"}</span>` : ""}</div>${live ? `<div id="liveDelivery" class="log-delivery" role="status">${esc(liveDeliverySummary(state.live, selected?.Burn, selected?.Step))}</div>` : ""}<div id="termBody" class="term-body${state.showExec ? " show-exec" : ""}">${terminal}</div></div>
      </div>
    </div>
  </section>`;
  const existing = app?.querySelector("[data-detail-run]");
  if (existing?.dataset.detailRun === run.ID) {
    const template = document.createElement("template");
    template.innerHTML = detailHTML;
    for (const selector of [".rh", "#runBanners", "#stepList", "#logHead", ".term-bar"]) {
      existing.querySelector(selector).innerHTML = template.content.querySelector(selector).innerHTML;
    }
    const preserve = live && state.live?.view?.body === previousTerm;
    if (!preserve) existing.querySelector(".terminal").replaceWith(template.content.querySelector(".terminal"));
  } else replaceApp(detailHTML);
  const termBody = document.getElementById("termBody");
  if (live && state.live?.view) {
    state.live.view.body = termBody;
    scheduleLiveFrame(state.live);
  } else if (state.live?.view) state.live.view = null;
  if (termBody && (live || selected)) termBody.scrollTop = stick ? termBody.scrollHeight : previousScroll;
  const stepList = document.getElementById("stepList");
  if (stepList) stepList.scrollTop = previousStepScroll;
  if (status === "running" || (live && !state.live?.done)) setAuto(listPollMs);
  else if (status === "queued") setAuto(listPollMs);
  else clearInterval(state.timer);
}

/* ---------- repositories ---------- */
function upstreamName(upstreamID) {
  const info = (state.status?.upstream_info || []).find(upstream => upstream.id === upstreamID);
  return info ? info.name : (upstreamID ? "#" + upstreamID : "--");
}
function historyStrip(runs) {
  return `<div class="history" aria-label="recent runs, oldest to newest">${runs.slice(0, 12).reverse().map(run => `<button type="button" class="hist ${statusKind(displayRunStatus(run))}" data-run-id="${esc(run.ID)}" title="${esc(`${shortSha(run.SHA)} · ${run.RefKind === "tag" ? "tag" : "branch"} ${run.Ref} · ${statusLabel(displayRunStatus(run))} · ${ago(runWhen(run))}`)}"></button>`).join("") || '<span class="meta">--</span>'}</div>`;
}
function orderedRepos() {
  return [...state.repos.filter(repo => !repo.ArchivedAt), ...state.repos.filter(repo => repo.ArchivedAt)];
}
async function renderRepos(seq) {
  setChrome("repos", "/ repos");
  await Promise.all([loadRepos(), loadStatus().catch(() => { })]);
  if (!currentRoute(seq)) return;
  const rows = orderedRepos().map(repo => {
    const runs = repo.LatestRuns || [];
    return { repo, runs, latest: runs[0] || null };
  });
  replaceApp(`
  <section class="screen">
    <div class="bar"><h1>Repositories</h1><span class="meta">${state.repos.length} configured</span></div>
    <div class="table repo-table">
      <div class="trow head"><div>repository</div><div>status</div><div>default branch</div><div>latest run</div><div>history</div><div>upstream</div></div>
      ${rows.map(({ repo, runs, latest }) => `<div class="trow" data-repo-runs="${esc(repoKey(repo))}"><div class="primary"><div class="repo-name">${esc(repoLabel(repo))}</div></div><div>${repo.ArchivedAt ? pill("archived", "cancel") : latest ? pill(statusLabel(latest.Status), statusKind(latest.Status)) : '<span class="meta">no runs</span>'}</div><div class="branch">${esc(repo.DefaultBranch || "--")}</div><div>${latest ? `<div class="branch">${esc(latest.Ref)}</div><span class="meta">${esc(ago(runWhen(latest)))}</span>` : '<span class="meta">--</span>'}</div><div>${historyStrip(runs)}</div><div class="clip">${esc(upstreamName(repo.UpstreamID))}</div></div>`).join("") || '<div class="empty static">No repositories configured.</div>'}
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
  if (state.issueQuery) query.set("query", state.issueQuery);
  if (state.issueCursor) query.set("before", String(state.issueCursor));
  return "/api/issues?" + query.toString();
}
function issueRow(issue) {
  return `<button type="button" class="issue-row" data-open-issue="${esc(issue.ID)}" aria-label="Open internal issue ${esc(issue.ID)}: ${esc(issue.Title)}"><span class="sha">#${esc(issue.ID)}</span><span>${pill(issue.Kind, issue.Kind === "ci" ? "run" : "pending")}</span><span>${pill(issue.State, issue.State === "open" ? "fail" : "pass")}</span><span class="clip">${esc(issue.RepoID ? repoName(issue.RepoID) : (issue.upstream || "workspace"))}</span><span class="branch clip">${esc(issue.Branch || "--")}</span><span class="issue-title" title="${esc(issue.Title)}">${esc(issue.Title)}</span><span title="occurrences">×${esc(issue.Occurrences || 1)}</span><span title="updated ${esc(fmtTime(issue.UpdatedAt))}">${esc(ago(issue.UpdatedAt))}</span></button>`;
}
async function renderIssues(seq) {
  setChrome("issues", "/ issues");
  let pending = null;
  try {
    if (!state.repos.length) await loadRepos();
    pending = ensureStatus();
    if (state.issueRepo && !isQualifiedSelector(state.issueRepo)) await pending;
  } catch { /* names fall back */ }
  state.issueRepo = normalizeRepoSelector(state.issueRepo, "");
  let page;
  try {
    page = await api(issueListURL());
  } catch (err) {
    if (!currentRoute(seq)) return;
    if (state.issueRepo && [400, 404, 409].includes(err.status)) { changeIssueFilter({ issueRepo: "" }); return; }
    replaceApp(`<section class="screen"><h1>Issues</h1><div class="error">${esc(err.message || err)}<br><button type="button" class="retry" data-action="issue-retry">Retry</button></div></section>`);
    return;
  }
  if (!currentRoute(seq)) return;
  rerenderWhenStatus(pending, seq);
  state.issuePage = page;
  const issues = page.Issues || [];
  const facets = page.counts || {};
  const total = page.total ?? issues.length;
  const issueStart = state.issueCursorHistory.length * 50 + 1;
  const kinds = [["", "all"], ["ci", "ci"], ["manual", "manual"]];
  const states = [["open", "open"], ["closed", "closed"], ["", "all"]];
  const kindCount = kind => kind === "" ? (facets.manual || 0) + (facets.ci || 0) : (facets[kind] || 0);
  const stateCount = issueState => issueState === "" ? (facets.open || 0) + (facets.closed || 0) : (facets[issueState] || 0);
  replaceApp(`
  <section class="screen">
    <div class="bar"><h1>Issues</h1><span class="meta">${facets.open || 0} open · ${facets.manual || 0} manual · ${facets.ci || 0} CI · ${facets.closed || 0} closed</span></div>
    <div class="toolbar">
      ${kinds.map(([value, label]) => `<button class="chip ${state.issueKind === value ? "on" : ""}" data-issue-kind="${esc(value)}">${esc(label)} <span class="chip-count">${kindCount(value)}</span></button>`).join("")}
      <span class="status-divider"></span>
      ${states.map(([value, label]) => `<button class="chip ${state.issueState === value ? "on" : ""}" data-issue-state="${esc(value)}">${esc(label)} <span class="chip-count">${stateCount(value)}</span></button>`).join("")}
      <label>repo <select data-action="issue-repo"><option value="">all</option>${state.repos.map(repo => `<option value="${esc(repoKey(repo))}" ${state.issueRepo === repoKey(repo) ? "selected" : ""}>${esc(repoLabel(repo))}</option>`).join("")}</select></label>
      <label>search <input type="search" data-action="issue-query" value="${esc(state.issueQuery)}" aria-label="Search issue title, body or branch"></label>
      <span class="count">${pageRange(issueStart, issues.length, total)}</span>
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
  document.getElementById("issueDialogMeta").textContent = `${issue.Kind} · ${issue.State} · ${issue.upstream || "workspace-global"}`;
  document.getElementById("issueDialogContent").innerHTML = `
    <h3>${esc(issue.Title || "Untitled issue")}</h3>
    <p class="meta">${esc(issue.RepoID ? repoName(issue.RepoID) : (issue.upstream || "workspace-global"))} ${issue.Branch ? "/ " + esc(issue.Branch) : ""} · Oberth internal ledger</p>
    <dl class="kv-grid">
      <dt>kind</dt><dd>${esc(issue.Kind)}</dd>
      <dt>state</dt><dd>${esc(issue.State)}</dd>
      <dt>upstream</dt><dd>${esc(issue.upstream || "workspace-global")}</dd>
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
    if (state.live && path !== state.live.routePath) closeLiveLog();
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
    case "toggle-exec": toggleExecutorLines(); break;
    case "clear-token": clearToken(); break;
    case "submit-token": submitToken(); break;
    case "close-issue": closeIssue(); break;
    case "issue-retry": route(); break;
    case "issue-older": {
      const next = state.issuePage?.NextBefore;
      if (next) { state.issueCursorHistory.push(state.issueCursor); state.issueCursor = next; route(); }
      break;
    }
    case "runs-older": {
      if (state.runNext) { state.runCursorHistory.push(state.runCursor); state.runStartHistory.push(state.runStart); state.runStart += state.runSourceCount; state.runCursor = state.runNext; route(); }
      break;
    }
    case "runs-newer": {
      if (state.runCursorHistory.length) { state.runCursor = state.runCursorHistory.pop(); state.runStart = state.runStartHistory.pop(); route(); }
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
  if (target instanceof HTMLSelectElement && target.dataset.action === "issue-repo") changeIssueFilter({ issueRepo: target.value });
  if (target instanceof HTMLInputElement && target.dataset.action === "issue-query") changeIssueFilter({ issueQuery: target.value.trim() });
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
  if ((target = event.target.closest("[data-repo-runs]"))) { changeRunRepo(target.dataset.repoRuns); go("/runs"); return; }
  if ((target = event.target.closest("[data-repo-filter]"))) { changeRunRepo(target.dataset.repoFilter); route(); return; }
  if ((target = event.target.closest("[data-kind-filter]"))) { state.releaseOnly = !state.releaseOnly; route(); return; }
  if ((target = event.target.closest("[data-status-filter]"))) { state.filter = target.dataset.statusFilter; route(); return; }
  if ((target = event.target.closest("[data-issue-kind]"))) { changeIssueFilter({ issueKind: target.dataset.issueKind }); return; }
  if ((target = event.target.closest("[data-issue-state]"))) { changeIssueFilter({ issueState: target.dataset.issueState }); return; }
  if ((target = event.target.closest("[data-select-step]")) && target.dataset.selectStep !== undefined) {
    state.step = target.dataset.selectStep || "__summary__";
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
    if (next !== current) { state.step = rows[next].dataset.selectStep || "__summary__"; route(true); }
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
