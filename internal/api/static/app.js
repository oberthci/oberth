"use strict";
/* Oberth dashboard — resurrected pre-rewrite SPA, adapted for the FAB
   architecture. Talks only to the authenticated read-only views:
   /api/runs, /api/runs/{id}, /api/runs/{id}/logs, /api/repos, /api/issues,
   /api/status. The bearer token persists in localStorage so navigation and
   reloads never re-prompt. No Argo, no reconciliation, no shadow-main. */

const app = document.getElementById("app");
const listPollMs = 4000;
const slowPollMs = 10000;
const state = {
  repos: [], runs: [], status: null,
  timer: null, routeSeq: 0,
  filter: localStorage.getItem("oberth-filter") || "all",
  repo: localStorage.getItem("oberth-repo") || "all",
  step: "", stepRunID: "",
  logCache: new Map(),
  issueKind: "", issueState: "open", issueRepo: "", issueCursor: 0, issueCursorHistory: [],
  issuePage: null, issueOpener: null,
  lastList: "/runs",
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

/* ---------- status vocabulary (FAB run + step states) ---------- */
function statusKind(status) {
  switch (String(status || "").toLowerCase()) {
    case "passed": return "pass";
    case "failed": case "timed_out": return "fail";
    case "running": return "run";
    case "queued": return "pending";
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
  setToken(value);
  route();
}
function clearToken() { setToken(null); setConn(false, "signed out"); showTokenPrompt(); }

async function api(url) {
  let response;
  try {
    response = await fetch(url, { cache: "no-store", headers: authHeaders() });
  } catch {
    setConn(false, "network unreachable");
    throw new Error("Oberth API is unreachable");
  }
  if (response.status === 401) {
    showTokenPrompt(hasToken() ? "Token rejected — enter a current uplink token." : "");
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
async function loadRuns() { state.runs = await api("/api/runs?limit=200") || []; }
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
  return `<div class="table runs-table"><div class="trow head"><div>status</div><div>run</div><div>sha</div><div>actor</div><div>when</div><div>message</div></div>${runs.map(run => `<div class="trow" data-run-id="${esc(run.ID)}"><div class="status-cell run-status">${runStatusCell(run)}</div><div class="primary"><div class="repo-name">${esc(repoName(run.RepoID))} ${releaseBadge(run)} ${supersededBadge(run)}</div><div class="branch">${esc(run.RefKind === "tag" ? "tag " : "")}${esc(run.Ref)}</div></div><div class="sha">${esc(shortSha(run.SHA))}</div><div class="clip" title="${esc(run.Actor)}">${esc(run.Actor || "--")}</div><div class="nowrap" title="${esc(fmtTime(runWhen(run)))}">${esc(ago(runWhen(run)))}<br><span class="meta">${esc(fmtDur(runDuration(run)))}</span></div><div class="clip" title="${esc(runMessage(run))}">${esc(runMessage(run))}</div></div>`).join("")}</div>`;
}
async function renderRuns(seq) {
  setChrome("runs", "/ runs");
  await Promise.all([loadRepos(), loadRuns()]);
  if (!currentRoute(seq)) return;
  localStorage.setItem("oberth-filter", state.filter);
  localStorage.setItem("oberth-repo", state.repo);
  const runs = state.runs, visible = runs.filter(runMatches), counts = repoCounts(runs);
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
        <div class="side-head">repositories</div>
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
    burn.kind = kinds.includes("fail") ? "fail" : kinds.includes("run") ? "run" : kinds.every(kind => kind === "cancel") ? "cancel" : "pass";
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
function logLineClass(line) {
  const l = String(line || "").toLowerCase();
  if (l.includes("[fail]") || l.includes("failed") || l.includes("error") || l.includes("unavailable")) return "bad";
  if (l.includes("[pass]") || l.includes("success") || l.includes("succeeded") || l.includes("passed")) return "ok";
  if (l.includes("[pending]") || l.includes("pending") || l.includes("waiting") || l.includes("queued")) return "warn";
  if (l.startsWith("$ ") || l.startsWith("run ") || l.startsWith("step:")) return "info";
  return "";
}
function splitLogText(raw) {
  const clean = String(raw || "").replaceAll("\r\n", "\n").replaceAll("\r", "\n");
  if (!clean) return [];
  const lines = clean.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}
function renderLogLines(lines) {
  let out = "";
  for (let i = 0; i < lines.length; i++) {
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
  if (run.Release) lines.push("[release] reachable-tag release run — release burns executed with release credentials");
  const kind = statusKind(run.Status);
  if (kind === "pass") lines.push(`[pass] ${passed} step${passed === 1 ? "" : "s"} passed across ${burns} burn${burns === 1 ? "" : "s"}`);
  else if (kind === "fail") lines.push(`[fail] failed at ${run.FailedBurn ? run.FailedBurn + " / " : ""}${run.FailedStep || "unknown step"} — ${passed} step${passed === 1 ? "" : "s"} passed`);
  else if (kind === "run") lines.push(`[pending] running in phase ${run.Phase || "ci"} — ${passed} passed so far`);
  else if (kind === "cancel") lines.push(`[interrupted] ${run.Reason || "run was interrupted"}`);
  else lines.push(`[pending] queued behind earlier work for this repository`);
  if (run.SupersededBy) lines.push(`superseded by ${run.SupersededBy}`);
  if (run.Error) lines.push("", `error: ${run.Error}`);
  const timed = steps.map(step => ({ step, seconds: spanSeconds(step.StartedAt, step.FinishedAt, false) })).filter(item => item.seconds > 0).sort((a, b) => b.seconds - a.seconds);
  if (timed.length) lines.push("", `slowest: ${timed.slice(0, 2).map(item => `${stepKey(item.step)} ${fmtDur(item.seconds)}`).join(" · ")}`);
  lines.push("", "select a step for its retained log");
  return lines;
}
async function stepLogContent(runID, step) {
  if (statusKind(step.Status) === "run") return renderLogLines([`step: ${stepKey(step)} · running`, "", "step is running — the retained log appears when the step completes"]);
  const cacheKey = `${runID}|${step.Burn}|${step.Step}|${step.FinishedAt || ""}`;
  if (state.logCache.has(cacheKey)) return state.logCache.get(cacheKey);
  let html;
  try {
    const payload = await api(`/api/runs/${enc(runID)}/logs?burn=${enc(step.Burn)}&step=${enc(step.Step)}`);
    const lines = splitLogText(payload.output);
    html = renderLogLines([`step: ${stepKey(step)} · ${statusLabel(step.Status)}${step.ExitCode ? ` · exit ${step.ExitCode}` : ""}`, ""].concat(lines.length ? lines : ["no retained step output"]));
  } catch (err) {
    html = renderLogLines([`step: ${stepKey(step)} · ${statusLabel(step.Status)}`, "", `step log unavailable: ${err.message || err}`]);
  }
  if (state.logCache.size > 64) state.logCache.clear();
  state.logCache.set(cacheKey, html);
  return html;
}
async function renderRunDetail(runID, seq) {
  setChrome("runs", "/ run");
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
  const terminal = selected ? await stepLogContent(run.ID, selected) : renderLogLines(summaryLines(detail));
  if (!currentRoute(seq)) return;
  setChrome("runs", `/ ${repository}`);
  replaceApp(`
  <section class="screen wide">
    <div class="rh">
      <div class="rh1">
        ${bigIcon(run.Status)}
        <span class="rh-name"><span class="rp">${esc(repository)} /</span> ${esc(run.Ref)}</span>
        <span class="v4b ${kind === "pass" ? "pass" : kind === "fail" ? "fail" : kind === "run" ? "run" : "dim"}">${esc(phaseLabel)}</span>
        ${releaseBadge(run)}
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
        <div class="sl-h"><span>STEPS · ${stepCount}${run.Release ? " · RELEASE" : ""}</span></div>
        <button class="v4row summary${selected ? "" : " sel"}" data-select-step=""><span class="ic ${kind === "pass" ? "g" : kind === "fail" ? "r" : kind === "run" ? "o" : "d"}">≡</span><span class="nm">Run summary</span><span class="dur">${esc(fmtDur(seconds))}</span></button>
        <div class="sl-sep"></div>
        ${burns.map(burn => `<div class="burn-h">${statusIcon(burn.kind === "pass" ? "passed" : burn.kind === "fail" ? "failed" : burn.kind === "run" ? "running" : "skipped")}<span>${esc(burn.name)}</span><span class="dur">${esc(fmtDur(burn.seconds))}</span></div>${burn.steps.map(step => { const key = stepKey(step), sk = statusKind(step.Status); return `<button class="v4row${key === selectedKey && selected ? " sel" : ""}${sk === "pending" ? " pend" : ""}" data-select-step="${esc(key)}"><span class="ic ${sk === "pass" ? "g" : sk === "fail" ? "r" : sk === "run" ? "o" : "d"}">${sk === "pass" ? "✓" : sk === "fail" ? "✗" : sk === "run" ? "◠" : sk === "cancel" ? "⊘" : "◌"}</span><span class="nm">${esc(step.Step)}</span><span class="dur">${sk === "cancel" ? "skipped" : esc(fmtDur(spanSeconds(step.StartedAt, step.FinishedAt, sk === "run")))}</span></button>`; }).join("")}`).join("") || '<div class="empty">No step results recorded yet.</div>'}
      </div>
      <div class="logp">
        <div class="log-h" id="logHead">${selected ? `${statusIcon(selected.Status)}<span class="nm">${esc(stepKey(selected))}</span><span class="v4b ${statusKind(selected.Status) === "pass" ? "pass" : statusKind(selected.Status) === "fail" ? "fail" : statusKind(selected.Status) === "run" ? "run" : "dim"}">${esc(statusLabel(selected.Status).toUpperCase())}</span>` : `<span class="ic ${kind === "pass" ? "g" : kind === "fail" ? "r" : kind === "run" ? "o" : "d"}">≡</span><span class="nm">Run summary</span><span class="v4b ${kind === "pass" ? "pass" : kind === "fail" ? "fail" : kind === "run" ? "run" : "dim"}">${esc(phaseLabel)} · ${esc(fmtDur(seconds))}</span>`}</div>
        <div class="terminal"><div class="term-bar"><span class="lamp red"></span><span class="lamp yellow"></span><span class="lamp green"></span><span class="term-title">${esc(repository)} / ${esc(run.Ref)} — ${selected ? esc(stepKey(selected)) : "summary"}</span></div><div id="termBody" class="term-body">${terminal}</div></div>
      </div>
    </div>
  </section>`);
  if (run.Status === "running" || run.Status === "queued") setAuto(listPollMs); else clearInterval(state.timer);
}

/* ---------- repositories ---------- */
function upstreamName(upstreamID) {
  const info = (state.status?.upstream_info || []).find(upstream => upstream.id === upstreamID);
  return info ? info.name : (upstreamID ? "#" + upstreamID : "--");
}
function historyStrip(runs) {
  return `<div class="history" aria-label="recent runs, oldest to newest">${runs.slice(0, 12).reverse().map(run => `<i class="${statusKind(run.Status)}" title="${esc(`${run.Ref} · ${statusLabel(run.Status)} · ${fmtTime(runWhen(run))}`)}"></i>`).join("") || '<span class="meta">--</span>'}</div>`;
}
async function renderRepos(seq) {
  setChrome("repos", "/ repos");
  await Promise.all([loadRepos(), loadRuns(), loadStatus().catch(() => { })]);
  if (!currentRoute(seq)) return;
  const rows = state.repos.map(repo => {
    const runs = state.runs.filter(run => run.RepoID === repo.ID);
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
    <div class="issue-body">${esc(issue.Body || "(empty)")}</div>`;
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
  const storeMood = !store || !store.configured ? "warn" : store.transport === "insecure-http" ? "warn" : "ok";
  const storeValue = !store || !store.configured ? "not configured" : store.transport === "insecure-http" ? "insecure http" : "configured";
  replaceApp(`
  <section class="screen">
    <div class="bar"><h1>Status</h1><span class="meta">FAB control plane health</span></div>
    <div class="svc-grid">
      ${svcCard("database", s.database || "unavailable", `${esc(s.upstreams ?? 0)} upstream${s.upstreams === 1 ? "" : "s"} · ${esc(s.repositories ?? 0)} repos`)}
      ${svcCard("upstream vcs", s.vcs || "unavailable", "")}
      ${svcCard("kubernetes", s.cluster || "unavailable", "")}
      ${svcCard("audit anchor", s.audit || "unavailable", chain && chain.detail ? esc(chain.detail) : "")}
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
      <div class="vcs-entry"><span class="vcs-led ${chain.anchor_id ? "ok" : "na"}"></span><span class="vcs-host">checkpoint</span><span class="meta">${chain.anchor_id ? "#" + esc(chain.anchor_id) : "none"}</span><span class="mono-detail">${esc(chain.tsa_url || "--")}</span><span class="mono-detail">${chain.anchored_at ? "anchored " + esc(ago(chain.anchored_at)) : "not yet anchored"}</span></div>
      ${chain.detail ? `<div class="vcs-entry"><span class="vcs-led err"></span><span class="vcs-host chain-bad">attention</span><span class="mono-detail chain-bad" title="${esc(chain.detail)}">${esc(chain.detail)}</span><span></span><span></span></div>` : ""}` : `<div class="vcs-entry"><span class="vcs-led na"></span><span class="meta">Audit chain detail is unavailable.</span></div>`}
    </div>
    ${store && store.configured ? `
    <div class="vcs-panel">
      <div class="vcs-head">Secret store (OpenBao)</div>
      <div class="vcs-entry"><span class="vcs-led ${store.transport === "insecure-http" ? "err" : "ok"}"></span><span class="vcs-host">address</span><span class="meta">${esc(store.transport)}</span><span class="mono-detail">${esc(store.address)}</span><span class="mono-detail">auth ${esc(store.auth_mount)} · role ${esc(store.role)}</span></div>
      <div class="vcs-entry"><span class="vcs-led na"></span><span class="vcs-host">verify</span><span class="meta">in-pod</span><span class="mono-detail">oberth secretstore verify</span><span class="mono-detail">exercises login, TokenReview, read policy, and TLS end to end</span></div>
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
    if (match) return await renderRunDetail(decodeURIComponent(match[1]), seq);
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
