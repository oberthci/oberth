"use strict";
const copy = {
 emptyRuns: ["No runs yet.", "Push a branch to start one."],
 filteredRuns: ["No runs match this filter.", "Choose all to see every run."],
 emptyRepos: ["No repositories yet.", "An operator adds one with oberth repo add."],
 emptyIssues: ["No open issues."], filteredIssues: ["No issues match this filter."],
 pastIssues: ["No issues on this page.", "Choose a filter to start from the newest issues."],
 accepted: ["Request accepted."], cancel: ["Cancel this run?"], abandon: ["Release this held step?"],
 emptySteps: ["This run has no planned steps."], emptyLog: ["This step wrote no output."],
 clearRepo: ["Select it again to show all repositories."],
 network: ["Oberth is not reachable.", "Check that the server is running, then reload this page."],
 reconnecting: ["Reconnecting to Oberth.", "Live updates retry by themselves."],
 ready: ["Server ready."], notReady: ["Server unavailable."], unknown: ["Server status."],
 live: ["Live updates stopped.", "They resume when the server answers again."],
 logBound: ["Earlier live output is outside this view.", "Use Earlier output to read the retained log."],
};
const main = document.querySelector("#main"), notice = document.querySelector("#notice");
const signin = document.querySelector("#signin");
let screen, serial = 0;
const readSpacing = 1000;
function h(tag, attrs = {}, ...children) {
 const node = document.createElement(tag);
 for (const [key, value] of Object.entries(attrs)) {
  if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
  else if (value !== null && value !== undefined) node.setAttribute(key, value);
 }
 node.append(...children.flat().filter(v => v !== null && v !== undefined));
 return node;
}
function button(text, onclick) { return h("button", {type: "button", onclick}, text); }
function detail(text) { return text ? h("details", {}, h("summary", {}, "Detail"), h("pre", {}, text)) : null; }
function empty(words) { return h("div", {class: "empty"}, words.map(word => h("p", {}, word))); }
function showError(error, target = notice) {
 if (error.name === "AbortError" || error.signin) return;
 const context = Array.isArray(error.context) ? error.context.map(text => h("pre", {}, String(text))) : [];
 const message = h("div", {}, ...(error.reason ? [h("p", {}, error.reason), h("p", {}, error.remedy || ""), ...context, detail(error.detail)] : [empty(copy.network)]));
 target.replaceChildren(message);
 return message;
}
async function exchange(path, options = {}) {
 let response, value;
 try {
  response = await fetch(path, {...options, credentials: "same-origin", redirect: "error", headers: {
    Accept: "application/json", ...(localStorage.getItem("oberth.token") ? {Authorization: "Bearer " + localStorage.getItem("oberth.token")} : {}), ...options.headers,
   }});
  if (!response.ok) value = await response.json();
 } catch (error) { if (error.name !== "AbortError") reached(false); throw error; }
 reached(!unavailable(response.status));
 if (!response.ok) {
  if (response.status === 401) {
   value.signin = !localStorage.getItem("oberth.token");
   signin.hidden = false; main.hidden = true; showError(value); screen?.controller.abort(); screen?.logs?.abort();
  }
  throw Object.assign(new Error(), value, {refused: true, status: response.status, retry: retryAfter(response.headers.get("Retry-After"))});
 }
 return response;
}
// Connection trouble (no answer, a non-JSON answer, or a gateway or drain status)
// shows only in the header light; background reads retry it quietly.
const unavailable = status => [502, 503, 504].includes(status);
const trouble = error => !error.reason || unavailable(error.status);
function retryAfter(value) { return /^\d+$/.test(value || "") ? Number(value) * 1000 : 5000; }
async function get(path, signal) { return (await exchange(path, {signal})).json(); }
async function call(name, args, signal) {
 const value = await (await exchange("/mcp", {method: "POST", signal, headers: {"Content-Type": "application/json"}, body: JSON.stringify({jsonrpc: "2.0", id: ++serial, method: "tools/call", params: {name, arguments: args}})})).json();
 if (value.error) throw Object.assign(new Error(), value.error.data || {reason: value.error.message}, {refused: true});
 if (value.result.isError) throw Object.assign(new Error(), JSON.parse(value.result.content[0].text), {refused: true});
 return value.result.structuredContent;
}
signin.querySelector("form").addEventListener("submit", event => {
 event.preventDefault(); localStorage.setItem("oberth.token", signin.querySelector("input").value); location.reload();
});
document.querySelector("#signout").addEventListener("click", () => { localStorage.removeItem("oberth.token"); location.reload(); });
function params(values) { return new URLSearchParams(Object.entries(values).filter(([,v]) => v !== undefined && v !== null && v !== "")).toString(); }
function time(value, now = Date.now()) {
 if (!value) return "";
 const date = new Date(value), today = new Date(now);
 const clock = date.toLocaleTimeString("en-GB", {hour: "2-digit", minute: "2-digit", hourCycle: "h23"});
 return (date.toDateString() === today.toDateString() ? "" : date.toLocaleDateString("en-US", {month: "short", day: "numeric"}) + " ") + clock;
}
function fullTime(value) { return new Date(value).toString() + " [" + value + "]"; }
function fullTimes(value) {
 return [["Created", value.created_at], ["Started", value.started_at], ["Finished", value.finished_at]]
  .filter(([, at]) => at).map(([label, at]) => label + ": " + fullTime(at)).join("\n");
}
function duration(start, end) {
 if (!start) return "";
 const seconds = Math.max(0, Math.floor(((end ? new Date(end).getTime() : Date.now()) - new Date(start).getTime()) / 1000));
 if (seconds < 60) return seconds + "s";
 if (seconds < 3600) return Math.floor(seconds / 60) + "m " + seconds % 60 + "s";
 return Math.floor(seconds / 3600) + "h " + Math.floor(seconds % 3600 / 60) + "m";
}
function clock(value) { return h("span", {class: "duration", "data-start": value.started_at, "data-end": value.finished_at}, duration(value.started_at, value.finished_at)); }
function when(run) {
 const at = run.started_at || run.created_at;
 return h("span", {class: "when", title: fullTimes(run)}, h("time", {datetime: at, title: fullTime(at)}, time(at)), run.started_at ? " · " : "", clock(run));
}
// A run's useful time on one line: when it finished and how long it took, when it
// started and how long it has run, or when it was queued.
function runWhen(run) {
 const at = run.finished_at || run.started_at || run.created_at, word = run.finished_at ? null : run.started_at ? "started " : "queued ";
 return h("span", {class: "when", title: fullTimes(run)}, word && h("span", {class: "at-word"}, word), h("time", {datetime: at, title: fullTime(at)}, time(at)), run.started_at ? [" · ", clock(run)] : null);
}
function refName(ref) { return ref.startsWith("refs/tags/") ? "tag " + ref.slice(10) : ref.replace(/^refs\/heads\//, ""); }
function state(status) { return h("span", {class: "state " + status}, h("span", {class: "mark " + status, "aria-hidden": "true"}), status + " "); }
function runIdentity(run) {
 return h("p", {class: "identity", "aria-label": "Run and commit"}, h("span", {title: "Run ID"}, "run " + run.id.slice(0, 8)), " · ", h("span", {title: "Commit ID"}, "commit " + (run.commit_sha ? run.commit_sha.slice(0, 12) : "unknown")));
}
function runOrigin(run) {
 const parts = [run.trigger && run.trigger + (run.schedule_entry ? " " + run.schedule_entry : ""), run.rerun_of && "rerun of " + run.rerun_of.slice(0, 8), run.actor && "by " + run.actor, run.assist_requested && "assist requested"].filter(Boolean);
 return parts.length ? h("p", {class: "muted", "aria-label": "Started by"}, parts.join(" · ")) : null;
}
function runDetail(run) {
 return h("details", {}, h("summary", {}, "Detail"), run.detail ? h("pre", {}, run.detail) : null,
  h("pre", {class: "mono"}, ["Run ID: " + run.id, "Object ID: " + run.object_sha, "Commit ID: " + (run.commit_sha || "unknown"), fullTimes(run)].filter(Boolean).join("\n")));
}
setInterval(() => document.querySelectorAll("[data-start]").forEach(node => { node.textContent = duration(node.dataset.start, node.dataset.end); }), 1000);
function replace(region, nodes) {
 const key = region.contains(document.activeElement) ? document.activeElement.dataset.key : null;
 region.replaceChildren(...nodes);
 if (key) Array.from(region.querySelectorAll("[data-key]")).find(node => node.dataset.key === key)?.focus({preventScroll: true});
}
function navigate(path) { history.pushState(null, "", path); route(); }
document.addEventListener("click", event => {
 const link = event.target.closest("a");
 if (event.defaultPrevented || !link || event.button || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || link.origin !== location.origin || link.hash) return;
 event.preventDefault(); navigate(link.pathname + link.search);
});
window.addEventListener("popstate", route);
function refresh(view) {
 view.dirty = true;
 view.reading ||= redraw(view).finally(() => { view.reading = null; });
 return view.reading;
}
async function redraw(view) {
 const signal = view.controller.signal;
 try {
  while (view.dirty && !signal.aborted) {
   const delay = (view.lastRead ?? -Infinity) + readSpacing - performance.now();
   if (delay > 0) await waitRetry(Math.ceil(delay), signal);
   if (signal.aborted || document.hidden) return false;
   view.dirty = false; view.lastRead = performance.now();
   await view.read();
   view.readError?.remove(); view.liveError?.remove();
  }
  if (view.actionAccepted && !signal.aborted) releaseActions(view);
  return !signal.aborted;
 } catch (error) { if (!signal.aborted && !trouble(error)) view.readError = showError(error); return false; }
}
function waitRetry(milliseconds, signal) {
 return new Promise(resolve => {
  const finish = () => { clearTimeout(timer); signal.removeEventListener("abort", finish); resolve(); };
  const timer = setTimeout(finish, milliseconds);
  signal.addEventListener("abort", finish, {once: true});
  if (signal.aborted) finish();
 });
}
async function live(view) {
 if (view.waiting || document.hidden || view.controller.signal.aborted) return;
 view.waiting = new AbortController();
 const stop = () => view.waiting?.abort();
 view.controller.signal.addEventListener("abort", stop, {once: true});
 const signal = view.waiting.signal;
 try {
  while (!signal.aborted) {
   try {
    const page = await call("events_wait", {...view.filter, after_id: view.cursor, timeout_s: 25, limit: 200}, signal);
    view.cursor = page.next_after_id; view.liveError?.remove(); view.readError?.remove();
    if (page.events.length || page.gap) {
     await waitRetry(readSpacing, signal);
     if (!signal.aborted) await refresh(view);
    }
   } catch (error) {
    if (signal.aborted) break;
    // Connection trouble is retried quietly (the header light shows it); a refusal is shown.
    view.liveError?.remove();
    if (!trouble(error)) {
     view.liveError = showError(error, h("div"));
     view.liveError.append(empty(copy.live)); notice.append(view.liveError);
    }
    await waitRetry(error.retry ?? 5000, signal);
    if (!signal.aborted) await refresh(view);
   }
  }
 } finally {
  view.controller.signal.removeEventListener("abort", stop); view.waiting = null;
  if (!document.hidden && !view.controller.signal.aborted) live(view);
 }
}
document.addEventListener("visibilitychange", async () => {
 if (!screen || (!screen.filter && !screen.dirty)) return;
 if (document.hidden) screen.waiting?.abort();
 else { const view = screen; await refresh(view); if (view.filter) live(view); }
});
async function route() {
 screen?.controller.abort(); screen?.logs?.abort();
 const view = {controller: new AbortController(), filter: {}}; screen = view;
 main.replaceChildren(); notice.replaceChildren();
 const parts = location.pathname.split("/").filter(Boolean);
 document.querySelectorAll("nav a").forEach(link => { if (link.pathname === "/" + parts[0]) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current"); });
 try {
  if (parts[0] === "runs" && parts[1]) runScreen(view, parts[1]);
  else if (parts[0] === "runs") await runsScreen(view);
  else if (parts[0] === "repos") reposScreen(view);
  else if (parts[0] === "issues") issuesScreen(view, parts[1]);
  else if (parts[0] === "status") statusScreen(view);
  if (view.controller.signal.aborted) return;
  await refresh(view); if (view.filter) live(view);
 } catch (error) {
  if (view.controller.signal.aborted) return;
  if (!trouble(error)) { showError(error); return; }
  await waitRetry(error.retry ?? 5000, view.controller.signal);
  if (!view.controller.signal.aborted) route();
 }
}
// A canonical name is upstream/organization/name. Lists show the name under an
// upstream/organization header, and more of the prefix only where two names
// would otherwise read the same.
const groupOf = repo => repo.split("/").slice(0, 2).join("/");
function shortNames(repos) {
 const cut = (repo, depth) => repo.split("/").slice(depth).join("/");
 return new Map(repos.map(repo => {
  let depth = 2;
  while (depth > 0 && repos.some(other => other !== repo && cut(other, depth) === cut(repo, depth))) depth--;
  return [repo, cut(repo, depth)];
 }));
}
// Collapsed side-list groups are a per-viewer convenience; the list works without storage.
function collapsedGroups() {
 try { return new Set(JSON.parse(localStorage.getItem("oberth.collapsed") || "[]")); } catch { return new Set(); }
}
function saveCollapsed(groups) {
 try { localStorage.setItem("oberth.collapsed", JSON.stringify([...groups])); } catch { /* the list still works */ }
}
// One disclosure per upstream/organization; collapsing hides its rows, not its runs.
// The group holding the chosen repository opens whatever was stored.
function repoGroup([group, members], index, labels, repo, link) {
 const collapsed = collapsedGroups(), open = !collapsed.has(group) || members.includes(repo), id = "repo-group-" + index;
 const rows = h("div", {class: "repo-members", id, role: "group", "aria-label": group, hidden: open ? null : ""},
  members.map(name => h("a", {class: "repo-filter", href: link("repo", name === repo ? "" : name), title: name === repo ? name + ". " + copy.clearRepo[0] : name, "aria-label": name, "aria-current": name === repo ? "true" : null}, labels.get(name))));
 const toggle = h("button", {type: "button", class: "repo-group", title: group, "aria-expanded": String(open), "aria-controls": id, onclick: () => {
  const stored = collapsedGroups(), closing = toggle.getAttribute("aria-expanded") === "true";
  toggle.setAttribute("aria-expanded", String(!closing)); rows.hidden = closing;
  if (closing) stored.add(group); else stored.delete(group);
  saveCollapsed(stored);
 }}, h("span", {class: "chevron", "aria-hidden": "true"}), h("span", {class: "repo-group-name"}, group));
 return [toggle, rows];
}
async function runsScreen(view) {
 const query = new URLSearchParams(location.search), signal = view.controller.signal;
 const repos = await get("/api/repos?names_only=true", signal);
 if (signal.aborted) return;
 const change = key => event => { query.delete("before"); query.set(key, event.target.value); navigate("/runs?" + query); };
 const link = (key, value) => "/runs?" + params({...Object.fromEntries(query), before: null, [key]: value});
 const repo = query.get("repo") || "", status = query.get("status") || "";
 const names = repos.repositories.map(item => item.repo).sort(), labels = shortNames(names), groups = [...Map.groupBy(names, groupOf)];
 main.append(h("h1", {}, "Runs"),
  h("aside", {class: "repo-side", "aria-label": "Repositories"},
   h("a", {class: "repo-group", href: link("repo", ""), "aria-current": repo ? null : "true"}, h("span", {class: "chevron", "aria-hidden": "true"}), h("span", {class: "repo-group-name"}, "All repos")), groups.flatMap((group, index) => repoGroup(group, index, labels, repo, link))),
  h("div", {class: "filters"},
   h("label", {}, "Repository", h("select", {onchange: change("repo")}, h("option", {value: ""}, "all repositories"),
    groups.map(([group, members]) => h("optgroup", {label: group}, members.map(name => h("option", {value: name, selected: name === repo ? "" : null}, labels.get(name))))))),
   h("div", {class: "chips", role: "group", "aria-label": "Status"}, ["", "queued", "running", "passed", "failed", "cancelled"].map(value => h("a", {class: "chip", href: link("status", value), "aria-current": value === status ? "true" : null}, value || "all")))));
 const region = h("div", {class: "runs"}); main.append(region, h("p", {class: "runs-foot"}, "Newest first · 50 runs per page"));
 view.read = async () => {
  const page = await get("/api/runs?" + params({limit: 50, repo: query.get("repo"), ref: query.get("ref"), status: query.get("status"), before: query.get("before")}), signal);
  if (signal.aborted) return;
  replace(region, page.runs.length ? [h("div", {class: "row head", "aria-hidden": "true"}, ["STATUS", "REPOSITORY", "REF", "WHEN", "MESSAGE"].map(text => h("span", {}, text))),
   ...page.runs.map(run => h("a", {class: "row", href: "/runs/" + encodeURIComponent(run.id), "data-key": run.id}, state(run.status), h("b", {class: "repo", title: run.repo}, labels.get(run.repo) || run.repo),
    h("span", {class: "ref", title: run.ref}, refName(run.ref)), runWhen(run), h("span", {class: "message", title: run.display_reason || null}, run.display_reason))),
   page.next_cursor ? h("a", {class: "pager", href: "/runs?" + params({...Object.fromEntries(query), before: page.next_cursor})}, "Older runs") : null].filter(Boolean) : [empty(query.size ? copy.filteredRuns : copy.emptyRuns)]);
 };
}
function runScreen(view, id) {
 const signal = view.controller.signal, path = "/api/runs/" + encodeURIComponent(id);
 view.filter = {run_ids: [id]};
 const header = h("section", {class: "run-head"}), steps = h("div", {class: "steps"}), pane = h("section", {class: "log", id: "log", "aria-label": "Step log"});
 main.append(h("a", {class: "back", href: "/runs"}, "< Runs"), header, h("div", {class: "run-layout"}, steps, pane));
 view.read = async () => {
  const run = await get(path + "?" + params({view: "dashboard", step_limit: 100, step_offset: view.offset}), signal);
  view.repositories ||= get("/api/repos?" + params({repo: run.repo}), signal).catch(error => { view.repositories = null; throw error; });
  const repositories = await view.repositories;
  const holds = await call("run_hold_list", {id}, signal).catch(() => ({holds: [], unknown: true}));
  if (signal.aborted) return;
  replace(header, [h("h1", {}, state(run.status), " ", h("span", {class: "mono"}, run.repo + " · " + run.ref)),
   h("p", {}, run.failure ? h("b", {class: "failed-step"}, run.failure.failed_step_name || run.failure.failed_step) : null, run.display_reason), runIdentity(run), runOrigin(run),
   h("p", {class: "muted"}, run.started_at ? "Started " : "Created ", when(run)), runDetail(run),
   h("a", {href: "/runs?" + params({repo: run.repo, ref: run.ref})}, "Other runs of this ref"),
   h("p", {}, (run.steps_planned_exact ? "" : ">=") + run.step_count + " steps · " + run.passed + " passed · " + run.failed + " failed · " + run.pending + " pending")].filter(Boolean));
  header.append(runActions(view, run, repositories.repositories, holds.holds));
  if (holds.unknown) header.append(h("p", {class: "muted"}, "hold availability unknown"));
  replace(steps, (run.steps || []).length ? [h("p", {class: "steps-head"}, "Steps · " + (run.steps_planned_exact ? "" : ">=") + run.step_count), ...stepRows(view, run.steps, path, pane)] : [empty(copy.emptySteps)]);
  if (run.next_step_offset !== undefined) steps.append(button("Next 100 steps", () => { view.offset = run.next_step_offset; refresh(view); }));
  if (view.offset) steps.append(button("First steps", () => { view.offset = 0; refresh(view); }));
  const selected = run.steps?.find(step => step.plan_path === view.selected);
  if (selected && view.logHeader) view.logHeader.replaceChildren(...[h("h2", {}, selected.name + " · " + selected.status), h("p", {}, selected.display_reason), detail(selected.detail)].filter(Boolean));
  if (view.selected === undefined && run.steps?.length) chooseStep(view, run.steps.find(step => step.status === "failed") || run.steps[0], path, pane, false);
  else if (!view.logBusy) view.logReload?.();
  else view.logDirty = true;
 };
}
function stepRows(view, steps, path, pane) {
 const rows = [];
 let group, box;
 for (const step of steps) {
  if (step.parallel_group && step.parallel_group !== group) {
   box = h("div", {class: "parallel-steps", "data-group": step.parallel_group});
   rows.push(box);
  }
  group = step.parallel_group;
  const row = stepRow(view, step, path, pane);
  if (group) box.append(row);
  else rows.push(row);
 }
 return rows;
}
function stepRow(view, step, path, pane) {
 return h("a", {class: "row", href: "#log", "data-key": step.plan_path, "aria-current": view.selected === step.plan_path ? "step" : null, onclick: event => {
  event.preventDefault(); chooseStep(view, step, path, pane, true);
 }}, state(step.status), h("span", {class: "mono step-name", title: step.name}, step.name), step.credential_intent ? h("span", {class: "secret"}, "secret") : h("span"), clock(step));
}
function chooseStep(view, step, path, pane, scroll) {
 view.selected = step.plan_path; view.logs?.abort(); view.logs = new AbortController();
 const stop = () => view.logs?.abort();
 view.controller.signal.addEventListener("abort", stop, {once: true});
 view.logs.signal.addEventListener("abort", () => view.controller.signal.removeEventListener("abort", stop), {once: true});
 view.logHeader = h("div", {}, h("h2", {}, step.name + " · " + step.status), h("p", {}, step.display_reason), detail(step.detail));
 pane.replaceChildren(view.logHeader);
 const controls = h("div", {class: "toolbar"}), status = h("div"), output = h("pre", {tabindex: "0"}), gutter = h("div", {class: "gutter", "aria-hidden": "true"}, "");
 pane.append(controls, status, h("div", {class: "terminal"}, gutter, output));
 const args = {step: step.plan_path, tail: true, limit: 500};
 const selectors = [...(step.logs || [])];
 const selection = h("select", {"aria-label": "Attempt or container", onchange: () => {
  const chosen = selectors[Number(selection.value)];
  loadLog(view, path, chosen ? {node_id: chosen.node_id, container: chosen.container} : args, {controls, status, output, gutter}, true);
 }}, h("option", {value: "-1"}, "All attempts"), selectors.map((item, index) => h("option", {value: index}, item.node_id + " · " + item.container)));
 controls.append(h("label", {}, "Container", selection));
 get(path + "/containers", view.logs.signal).then(page => {
  if (view.selected !== step.plan_path || view.controller.signal.aborted) return;
  for (const item of page.containers) {
   if (!step.attempts?.some(attempt => attempt.node_id === item.node_id) || selectors.some(existing => existing.node_id === item.node_id && existing.container === item.container)) continue;
   selectors.push(item); selection.append(h("option", {value: selectors.length - 1}, item.node_id + " · " + item.container));
  }
 }).catch(error => { if (!view.controller.signal.aborted && !trouble(error)) showError(error); });
 loadLog(view, path, args, {controls, status, output, gutter}, false);
 document.querySelectorAll(".steps [data-key]").forEach(node => { if (node.dataset.key === step.plan_path) node.setAttribute("aria-current", "step"); else node.removeAttribute("aria-current"); });
 if (scroll && matchMedia("(max-width: 759px)").matches) pane.scrollIntoView({behavior: "instant"});
}
function releaseActions(view) {
 view.actionPending = false; view.actionAccepted = false;
 for (const node of view.actionButtons || []) node.disabled = false;
}
function action(label, task, view, options = {}) {
 const node = button(label, async () => {
  if (node.disabled || view.actionPending || (options.question && !confirm(options.question))) return;
  node.disabled = true; view.actionPending = true;
  for (const other of view.actionButtons || []) if (other !== node) other.disabled = true;
  try {
   const receipt = await task();
   if (view.controller.signal.aborted) return;
   if (options.rerun && receipt.id) navigate("/runs/" + encodeURIComponent(receipt.id));
   else { notice.replaceChildren(empty(copy.accepted), h("p", {}, receipt.warning || "")); view.actionAccepted = true; await refresh(view); }
  } catch (error) {
   if (!view.controller.signal.aborted) showError(error);
   if (error.refused) { releaseActions(view); node.disabled = false; }
  }
 });
 node.disabled = Boolean(view.actionPending);
 (view.actionButtons ||= []).push(node);
 return node;
}
function runActions(view, run, repositories, holds) {
 view.actionButtons = [];
 const region = h("div", {class: "toolbar"}), signal = view.controller.signal;
 const args = {id: run.id, reason: "web UI"};
 if (["queued", "running"].includes(run.status)) region.append(action("Cancel", () => call("run_cancel", args, signal), view, {question: copy.cancel[0]}));
 if (["passed", "failed", "cancelled"].includes(run.status)) region.append(action("Rerun", () => call("run_rerun", args, signal), view, {rerun: true}));
 const repository = repositories.find(repo => repo.repo === run.repo);
 if (run.status === "passed" && run.commit_sha && repository?.default_branch && refName(run.ref) !== refName(repository.default_branch)) region.append(action("Promote", () => call("promote", {repo: run.repo, sha: run.commit_sha, reason: "web UI"}, signal), view));
 for (const hold of holds) region.append(h("span", {}, "Step " + (hold.step || "main") + ": " + (hold.availability_unknown ? "availability unknown" : "held since " + time(hold.held_since) + " · about " + Math.max(0, Math.ceil((hold.remaining_ms || 0) / 1000)) + "s left")),
  action("Release", () => call("run_hold_abandon", {...args, step: hold.step}, signal), view, {question: copy.abandon[0]}));
 return region;
}
function reposScreen(view) {
 const query = new URLSearchParams(location.search), region = h("div", {class: "repos"});
 main.append(h("h1", {}, "Repositories"), h("label", {class: "toolbar"}, h("input", {type: "checkbox", checked: query.get("archived") === "include" ? "" : null, onchange: event => navigate("/repos" + (event.target.checked ? "?archived=include" : ""))}), "Show archived"), region);
 view.read = async () => {
  const page = await get("/api/repos?" + params({archived: query.get("archived"), recent_runs: true}), view.controller.signal);
  if (view.controller.signal.aborted) return;
  replace(region, page.repositories.length ? [h("div", {class: "row head"}, ["REPOSITORY", "DEFAULT BRANCH", "LATEST RUN", "LAST 20", "BRANCHES"].map(text => h("span", {}, text))),
   ...page.repositories.map(repoRow)] : [empty(copy.emptyRepos)]);
 };
}
// A repository row links to its run list. Its name is its text cells: the bars
// inside it are links of their own, and a link's name would otherwise take in
// every bar's title.
function repoRow(repo) {
 const cells = [h("span", {class: "mono"}, repo.repo), h("span", {class: "mono"}, repo.default_branch && refName(repo.default_branch)),
  h("span", {}, repo.latest_run ? [state(repo.latest_run.status), " · " + refName(repo.latest_run.ref) + " · " + time(repo.latest_run.created_at)] : "no runs"),
  h("span", {}, repo.branch_count === null ? "unknown" : String(repo.branch_count))];
 return h("a", {class: "row", href: "/runs?" + params({repo: repo.repo}), "data-key": repo.repo, "aria-label": cells.map(cell => cell.textContent.replace(/\s+/g, " ").trim()).filter(Boolean).join(" ")},
  cells.slice(0, 3), recentRuns(repo.recent_runs || []), cells[3]);
}
// A repository's last runs as bars, oldest left: green passed, red failed, yellow
// queued or running, grey cancelled. Each bar opens its run and names it in its
// title. The bars sit inside the row's link, which stays the one stop for the
// keyboard and for assistive technology: the strip is hidden from the tree, the
// row's name leaves it out, and the bars take no tab stops.
const barColours = {passed: "passed", failed: "failed", queued: "running", running: "running"};
function recentRuns(runs) {
 return h("span", {class: "recent", "aria-hidden": "true"}, runs.map(run => {
  const words = run.status + " · " + refName(run.ref) + " · " + time(run.created_at);
  return h("a", {class: "bar " + (barColours[run.status] || "other"), href: "/runs/" + encodeURIComponent(run.id), title: words, tabindex: "-1"});
 }));
}
function statusScreen(view) {
 main.append(h("h1", {}, "Status"));
 const region = h("section"); main.append(region);
 view.read = async () => {
  const [status, counts] = await Promise.all([get("/api/status", view.controller.signal), get("/api/runs/counts", view.controller.signal)]);
  if (view.controller.signal.aborted) return;
  const ready = status.ready ? "passed" : "failed";
  region.replaceChildren(h("div", {class: "cards"},
   h("div", {class: "card"}, h("p", {class: "card-name"}, h("span", {class: "mark " + ready, "aria-hidden": "true"}), "Server"), h("p", {class: "card-value " + ready}, status.ready ? "ready" : "unavailable"),
    h("p", {class: "muted"}, "version " + status.version), h("p", {}, status.display_reason), detail(status.detail) || h("span")),
   ...Object.entries(counts).map(([name, count]) => h("div", {class: "card"}, h("p", {class: "card-name"}, h("span", {class: "mark " + name, "aria-hidden": "true"}), "Runs " + name), h("p", {class: "card-value " + name}, String(count))))));
 };
}
const themes = ["auto", "light", "dark", "mono", "color"], themeButton = document.querySelector("#theme");
function theme(value) {
 document.documentElement.dataset.theme = value;
 themeButton.textContent = "Theme: " + value; localStorage.setItem("oberth.theme", value);
}
theme(themes.includes(localStorage.getItem("oberth.theme")) ? localStorage.getItem("oberth.theme") : "auto");
themeButton.addEventListener("click", () => theme(themes[(themes.indexOf(document.documentElement.dataset.theme) + 1) % themes.length]));
// Escape leaves an issue for the issue list it came from, and on a run list
// filtered to one repository drops only that filter.
function escapeTarget() {
 if (location.pathname.startsWith("/issues/")) return "/issues" + location.search;
 if (location.pathname.startsWith("/issues")) return "/issues";
 const query = new URLSearchParams(location.search);
 if (location.pathname !== "/runs" || !query.has("repo")) return "/runs";
 query.delete("repo"); query.delete("before");
 return "/runs" + (query.size ? "?" + query : "");
}
document.addEventListener("keydown", event => {
 if (event.ctrlKey || event.metaKey || event.altKey || event.shiftKey || event.target.closest("input,textarea,select,[contenteditable]")) return;
 const paths = ["/runs", "/repos", "/issues", "/status"];
 if (/^[1-4]$/.test(event.key)) { event.preventDefault(); navigate(paths[Number(event.key) - 1]); }
 else if (event.key === "Escape") { event.preventDefault(); navigate(escapeTarget()); }
 else if (["ArrowLeft", "ArrowRight"].includes(event.key) && main.querySelector(`.pages a[aria-keyshortcuts="${event.key}"]`)) { event.preventDefault(); main.querySelector(`.pages a[aria-keyshortcuts="${event.key}"]`).click(); }
 else if (["j", "k", "ArrowDown", "ArrowUp"].includes(event.key)) {
  const rows = Array.from(main.querySelectorAll("a.row")), index = rows.indexOf(document.activeElement);
  const next = Math.max(0, Math.min(rows.length - 1, index + (["j", "ArrowDown"].includes(event.key) ? 1 : -1)));
  if (rows[next]) { event.preventDefault(); rows[next].focus(); }
 }
});
// The top bar's light carries the connection: normal; reconnecting after two failed
// requests in a row; unreachable once requests have failed for 30 s. Any answer resets it,
// and the version and readiness come from the status read the Status page makes.
const server = document.querySelector("#server"), connection = {failures: 0, since: 0, ready: null}, unreachableAfter = 30000;
function reached(ok) {
 const was = server.dataset.state || "normal";
 if (ok) connection.failures = 0;
 else if (!connection.failures++) connection.since = performance.now();
 server.dataset.state = ok ? "normal" : performance.now() - connection.since >= unreachableAfter ? "unreachable" : connection.failures > 1 ? "reconnecting" : "normal";
 paintServer();
 if (ok && was !== "normal") readStatus();
}
function paintServer() {
 const state = server.dataset.state || "normal", ready = connection.ready;
 const words = state === "unreachable" ? copy.network : state === "reconnecting" ? copy.reconnecting : ready === null ? copy.unknown : ready ? copy.ready : copy.notReady;
 server.querySelector(".mark").className = "mark " + (state === "reconnecting" ? "running" : state === "unreachable" || ready === false ? "failed" : ready ? "passed" : "");
 server.title = words.join(" ");
 server.setAttribute("aria-label", words.join(" ") + " Version " + (server.querySelector(".ver").textContent || "unknown") + ".");
}
function readStatus() {
 get("/api/status").then(status => { connection.ready = status.ready; server.querySelector(".ver").textContent = status.version; paintServer(); }).catch(() => {});
}
server.querySelector(".reload").addEventListener("click", event => { event.preventDefault(); location.reload(); });
if (localStorage.getItem("oberth.token")) readStatus();
route();
