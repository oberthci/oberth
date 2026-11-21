"use strict";
const copy = {
 emptyRuns: ["No runs yet.", "Push a branch to start one."],
 filteredRuns: ["No runs match this filter.", "Choose all to see every run."],
 emptyRepos: ["No repositories yet.", "An operator adds one with oberth repo add."],
 emptyIssues: ["No open issues."], filteredIssues: ["No issues match this filter."],
 pastIssues: ["No issues on this page.", "Choose a filter to start from the newest issues."],
 accepted: ["Request accepted."], cancel: ["Cancel this run?"], abandon: ["Release this held step?"],
 emptySteps: ["This run has no planned steps."], emptyLog: ["This step wrote no output."],
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
// upstream/organization header, and more of the prefix only where two visible
// names would otherwise read the same.
const upstreamOf = repo => repo.split("/")[0];
const groupOf = repo => repo.split("/").slice(0, 2).join("/");
function shortNames(repos) {
 const cut = (repo, depth) => repo.split("/").slice(depth).join("/");
 return new Map(repos.map(repo => {
  let depth = 2;
  while (depth > 0 && repos.some(other => other !== repo && cut(other, depth) === cut(repo, depth))) depth--;
  return [repo, cut(repo, depth)];
 }));
}
// One checkbox per upstream; the address holds the selection (absent means all).
// The last checked box cannot be cleared, so the list never selects nothing.
function upstreamChoice(query, upstreams, chosen) {
 const choose = (name, on) => {
  const next = upstreams.filter(item => item === name ? on : chosen.includes(item));
  query.delete("before");
  if (next.length === upstreams.length) query.delete("upstreams"); else query.set("upstreams", next.join(","));
  if (query.get("repo") && !next.includes(upstreamOf(query.get("repo")))) query.delete("repo");
  navigate("/runs?" + query);
 };
 const checked = upstreams.filter(name => chosen.includes(name));
 return h("div", {class: "upstreams", role: "group", "aria-label": "Upstreams"}, h("span", {class: "side-word", "aria-hidden": "true"}, "Upstreams"),
  upstreams.map(name => h("label", {}, h("input", {type: "checkbox", checked: chosen.includes(name) ? "" : null, disabled: checked.length === 1 && checked[0] === name ? "" : null, onchange: event => choose(name, event.target.checked)}), name)));
}
async function runsScreen(view) {
 const query = new URLSearchParams(location.search), signal = view.controller.signal;
 const repos = await get("/api/repos?names_only=true", signal);
 if (signal.aborted) return;
 const change = key => event => { query.delete("before"); query.set(key, event.target.value); navigate("/runs?" + query); };
 const link = (key, value) => "/runs?" + params({...Object.fromEntries(query), before: null, [key]: value});
 const repo = query.get("repo") || "", status = query.get("status") || "";
 const names = repos.repositories.map(item => item.repo).sort(), upstreams = [...new Set(names.map(upstreamOf))];
 const chosen = query.has("upstreams") ? query.get("upstreams").split(",") : upstreams;
 const visible = names.filter(name => chosen.includes(upstreamOf(name))), labels = shortNames(visible), groups = [...Map.groupBy(visible, groupOf)];
 main.append(h("h1", {}, "Runs"),
  upstreams.length > 1 ? upstreamChoice(query, upstreams, chosen) : null,
  h("aside", {class: "repo-side", "aria-label": "Repositories"}, h("p", {class: "side-head"}, "Repositories"),
   h("a", {class: "repo-filter", href: link("repo", ""), "aria-current": repo ? null : "true"}, "All repos"),
   groups.flatMap(([group, members]) => [h("p", {class: "repo-group", title: group}, group), ...members.map(name =>h("a", {class: "repo-filter", href: link("repo", name), title: name, "aria-label": name, "aria-current": name === repo ? "true" : null}, labels.get(name)))])),
  h("div", {class: "filters"},
   h("label", {}, "Repository", h("select", {onchange: change("repo")}, h("option", {value: ""}, "all"),
    groups.map(([group, members]) => h("optgroup", {label: group}, members.map(name => h("option", {value: name, selected: name === repo ? "" : null}, labels.get(name))))))),
   h("div", {class: "chips", role: "group", "aria-label": "Status"}, ["", "queued", "running", "passed", "failed", "cancelled"].map(value => h("a", {class: "chip", href: link("status", value), "aria-current": value === status ? "true" : null}, value || "all")))));
 const region = h("div", {class: "runs"}); main.append(region, h("p", {class: "runs-foot"}, "Newest first · 50 runs per page"));
 view.read = async () => {
  const page = await get("/api/runs?" + params({limit: 50, repo: query.get("repo"), ref: query.get("ref"), status: query.get("status"), upstreams: query.get("upstreams"), before: query.get("before")}), signal);
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
  replace(steps, (run.steps || []).length ? [h("p", {class: "steps-head"}, "Steps · " + (run.steps_planned_exact ? "" : ">=") + run.step_count), ...run.steps.map(step => stepRow(view, step, path, pane))] : [empty(copy.emptySteps)]);
  if (run.next_step_offset !== undefined) steps.append(button("Next 100 steps", () => { view.offset = run.next_step_offset; refresh(view); }));
  if (view.offset) steps.append(button("First steps", () => { view.offset = 0; refresh(view); }));
  const selected = run.steps?.find(step => step.plan_path === view.selected);
  if (selected && view.logHeader) view.logHeader.replaceChildren(...[h("h2", {}, selected.name + " · " + selected.status), h("p", {}, selected.display_reason), detail(selected.detail)].filter(Boolean));
  if (view.selected === undefined && run.steps?.length) chooseStep(view, run.steps.find(step => step.status === "failed") || run.steps[0], path, pane, false);
  else if (!view.logBusy) view.logReload?.();
  else view.logDirty = true;
 };
}
function stepRow(view, step, path, pane) {
 return h("a", {class: "row", href: "#log", "data-key": step.plan_path, "aria-current": view.selected === step.plan_path ? "step" : null, onclick: event => {
  event.preventDefault(); chooseStep(view, step, path, pane, true);
 }}, state(step.status), h("span", {class: "mono"}, step.name), step.credential_intent ? h("span", {class: "secret"}, "secret") : h("span"), clock(step));
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
async function loadLog(view, path, args, regions, restart) {
 if (restart) { view.logs.abort(); view.logs = new AbortController(); }
 const signal = view.logs.signal;
 view.logBusy = true; view.logDirty = false; view.logReload = () => loadLog(view, path, args, regions, false);
 try {
  while (!signal.aborted) {
   try {
    const native = args.node_id !== undefined;
    const result = await get(path + (native ? "/containerlog?" : "/logs?") + params({tail: true, limit: 500, ...args}), signal);
    if (signal.aborted) return;
    resetLog(regions, result.content || "");
    regions.status.replaceChildren(...[...(result.containers || []).map(item => h("p", {}, "Container: " + item.container)), h("p", {}, result.display_reason || result.meta.reason || (result.content ? "" : copy.emptyLog[0])), detail(result.detail || result.meta.detail)].filter(Boolean));
    regions.controls.querySelector("button")?.remove();
    if (result.meta.has_more) regions.controls.append(button("Earlier output", () => loadLog(view, path, {...args, byte_offset: result.meta.next_byte_offset}, regions, true)));
    const target = native ? args : result.containers?.at(-1);
    if (result.meta.copying || (result.meta.follow && args.byte_offset === undefined && !target)) { await waitRetry(250, signal); continue; }
    if (!result.meta.follow || args.byte_offset !== undefined || !target) return;
    await streamLog(path, target, {regions, signal});
   } catch (error) {
    if (signal.aborted) return;
    if (!trouble(error)) showError(error);
    await waitRetry(error.retry || 5000, signal);
   }
  }
 } finally {
  if (view.logs.signal === signal) {
   view.logBusy = false;
   if (view.logDirty && !signal.aborted) view.logReload();
  }
 }
}
async function streamLog(path, target, context) {
 const {regions, signal} = context;
 const response = await exchange(path + "/containerlog/stream?" + params({node_id: target.node_id, container: target.container}), {signal});
 const reader = response.body.getReader(), decoder = new TextDecoder();
 try {
  while (!signal.aborted) {
   const part = await reader.read();
   appendLive(regions, decoder.decode(part.value, {stream: !part.done}));
   if (part.done) break;
  }
 } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
}
function resetLog(regions, text) {
 regions.output.replaceChildren(); regions.liveBytes = 0; regions.lines = 0; regions.open = false;
 if (regions.gutter) { regions.gutter.firstChild.data = ""; regions.gutter.lines = 0; }
 appendLive(regions, text);
}
function newlines(text) {
 let count = 0;
 for (let index = text.indexOf("\n"); index >= 0; index = text.indexOf("\n", index + 1)) count++;
 return count;
}
// The gutter is one text node of line numbers beside the log; it is never part of the log's text.
function numbersLength(count) {
 let length = 0;
 for (let digits = 1, first = 1; first <= count; digits++, first *= 10) length += (Math.min(count, first * 10 - 1) - first + 1) * (digits + 1);
 return length;
}
function numberLines(regions) {
 const gutter = regions.gutter;
 if (!gutter) return;
 const total = regions.lines + (regions.open ? 1 : 0), node = gutter.firstChild;
 if (total < gutter.lines) node.deleteData(numbersLength(total), node.length);
 else if (total > gutter.lines) {
  const numbers = [];
  for (let line = gutter.lines + 1; line <= total; line++) numbers.push(line + "\n");
  node.appendData(numbers.join(""));
 }
 gutter.lines = total;
}
function appendLive(regions, text) {
 if (regions.liveBytes === undefined) { const initial = regions.output.textContent; resetLog(regions, initial); }
 const encoder = new TextEncoder(), decoder = new TextDecoder(), bytes = encoder.encode(text);
 for (let offset = 0; offset < bytes.length;) {
  let tail = regions.output.lastChild;
  if (!tail || tail.logBytes > 4092) { tail = document.createTextNode(""); tail.logBytes = 0; regions.output.append(tail); }
  let end = Math.min(offset + 4096 - tail.logBytes, bytes.length);
  while (end < bytes.length && (bytes[end] & 0xc0) === 0x80) end--;
  tail.appendData(decoder.decode(bytes.subarray(offset, end)));
  tail.logBytes += end - offset; regions.liveBytes += end - offset; offset = end;
 }
 regions.lines = (regions.lines || 0) + newlines(text);
 if (text) regions.open = !text.endsWith("\n");
 if (regions.liveBytes > 1048576) {
  regions.status.replaceChildren(empty(copy.logBound));
  while (regions.liveBytes > 1048576) {
   const head = regions.output.firstChild;
   let cut = regions.liveBytes - 1048576;
   if (cut >= head.logBytes) { regions.liveBytes -= head.logBytes; regions.lines -= newlines(head.data); head.remove(); continue; }
   const bytes = encoder.encode(head.data), before = head.data;
   while (cut < bytes.length && (bytes[cut] & 0xc0) === 0x80) cut++;
   head.data = decoder.decode(bytes.subarray(cut)); head.logBytes -= cut; regions.liveBytes -= cut;
   regions.lines -= newlines(before.slice(0, before.length - head.data.length));
  }
 }
 numberLines(regions);
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
  const page = await get("/api/repos?" + params({archived: query.get("archived")}), view.controller.signal);
  if (view.controller.signal.aborted) return;
  replace(region, page.repositories.length ? [h("div", {class: "row head"}, ["REPOSITORY", "DEFAULT BRANCH", "LATEST RUN", "BRANCHES"].map(text => h("span", {}, text))),
   ...page.repositories.map(repo => h("a", {class: "row", href: "/runs?" + params({repo: repo.repo}), "data-key": repo.repo}, h("span", {class: "mono"}, repo.repo), h("span", {class: "mono"}, repo.default_branch),
    h("span", {}, repo.latest_run ? [state(repo.latest_run.status), " · " + repo.latest_run.ref + " · " + time(repo.latest_run.created_at)] : "unknown"), h("span", {}, repo.branch_count === null ? "unknown" : String(repo.branch_count))))] : [empty(copy.emptyRepos)]);
 };
}
// The issue list is the agents' workspace. Each chip carries its count and
// chooses one filter, search narrows by text, and Newer and Older page by
// cursor. The address holds the whole choice; an issue's page links back to it.
const issueChips = [["all open", "open", {}], ["todo", "todo", {work_status: "todo"}], ["active", "active", {work_status: "active"}], ["waiting", "waiting", {work_status: "waiting"}],
 ["review", "review", {work_status: "review"}], ["stale", "stale", {stale: "true"}], ["closed", "closed", {state: "closed"}]];
const issueChoice = query => Object.fromEntries(["state", "work_status", "stale", "query", "before", "after"].map(key => [key, query.get(key)]));
const chosenChip = query => query.get("state") === "closed" ? "closed" : query.get("stale") === "true" ? "stale" : query.get("work_status") || "all open";
const number = value => (value || 0).toLocaleString("en-US");
function span(milliseconds, up) {
 const round = up ? Math.ceil : Math.floor, seconds = round(Math.max(0, milliseconds) / 1000);
 if (seconds < 60) return seconds + "s";
 if (seconds < 3600) return round(seconds / 60) + "m";
 return seconds < 172800 ? round(seconds / 3600) + "h" : round(seconds / 86400) + "d";
}
function workChip(word) { return h("span", {class: "state work " + word}, h("span", {class: "mark " + word, "aria-hidden": "true"}), word); }
function touched(issue, now) {
 if (issue.stale) return h("span", {class: "touched stale"}, "stale · " + span(now - Date.parse(issue.updated_at)) + " untouched");
 return h("span", {class: "touched"}, issue.closed_at ? "closed " + span(now - Date.parse(issue.closed_at)) + " ago" : "updated " + span(now - Date.parse(issue.updated_at)) + " ago");
}
function lockLine(issue, now) {
 return issue.lock ? h("span", {class: "lock", title: "Lock expires " + fullTime(issue.lock.expires_at)}, "locked by " + issue.lock.owner + " · " + span(Date.parse(issue.lock.expires_at) - now, true) + " left") : null;
}
const issuePlace = issue => issue.repo || (issue.upstream && issue.upstream !== "global" ? issue.upstream : "global");
function issueRow(issue, names, back, now) {
 const place = issuePlace(issue);
 return h("a", {class: "row", href: "/issues/" + issue.id + back, "data-key": String(issue.id)},
  h("span", {class: "issue-id"}, h("b", {}, "#" + issue.id), h("small", {class: issue.kind}, issue.kind === "ci" ? "CI" : "manual")),
  h("span", {class: "issue-main"}, h("span", {class: "title"}, issue.title), h("span", {class: "place", title: place}, names.get(place) || place)),
  h("span", {class: "issue-work"}, workChip(issue.state === "closed" ? "closed" : issue.work_status), lockLine(issue, now)),
  h("span", {class: "issue-when"}, touched(issue, now), h("time", {datetime: issue.updated_at, title: fullTime(issue.updated_at)}, time(issue.updated_at))));
}
function issuePager(page, choice, label) {
 const link = (text, cursor, direction, key) => cursor ? h("a", {href: "/issues?" + params({...choice, before: null, after: null, [direction]: cursor}), "aria-keyshortcuts": key}, text) : h("span", {"aria-disabled": "true"}, text);
 const shown = page.issues.length ? number(page.offset + 1) + "-" + number(page.offset + page.issues.length) : "0";
 return h("div", {class: "pages", role: "group", "aria-label": label}, h("span", {class: "position"}, shown + " of " + number(page.total)),
  link("< Newer", page.next_after, "after", "ArrowLeft"), link("Older >", page.next_before, "before", "ArrowRight"));
}
// Search waits for a pause in typing, then rewrites the address and rereads the list in place.
function searchIssues(view, text) {
 view.typing?.abort();
 const typing = view.typing = new AbortController(), stop = () => typing.abort();
 view.controller.signal.addEventListener("abort", stop, {once: true});
 waitRetry(300, typing.signal).then(() => {
  view.controller.signal.removeEventListener("abort", stop);
  if (typing.signal.aborted) return;
  const query = new URLSearchParams(location.search);
  query.delete("before"); query.delete("after");
  if (text.trim()) query.set("query", text.trim()); else query.delete("query");
  history.replaceState(null, "", "/issues" + (query.size ? "?" + query : ""));
  refresh(view);
 });
}
function issuesScreen(view, id) {
 view.filter = null;
 if (id) return issueScreen(view, id);
 const signal = view.controller.signal, chips = h("div", {class: "chips", role: "group", "aria-label": "Issue status"}), top = h("div"), bottom = h("div"), region = h("div", {class: "issues"});
 const search = h("input", {type: "search", class: "issue-search", "aria-label": "Search issues", placeholder: "Search titles and bodies", value: new URLSearchParams(location.search).get("query") || "", oninput: () => searchIssues(view, search.value)});
 main.append(h("h1", {}, "Issues"), h("div", {class: "filters"}, chips, search, button("Reload", () => refresh(view))), top, region, bottom);
 view.names = get("/api/repos?names_only=true", signal).then(page => shortNames(page.repositories.map(item => item.repo)), () => new Map());
 view.read = async () => {
  const query = new URLSearchParams(location.search), choice = issueChoice(query);
  const [page, names] = await Promise.all([get("/api/issues?" + params({limit: 50, ...choice}), signal), view.names]);
  if (signal.aborted) return;
  const chosen = chosenChip(query), now = Date.now();
  replace(chips, issueChips.map(([label, key, filter]) => h("a", {class: "chip", href: "/issues?" + params({query: choice.query, ...filter}), "aria-current": label === chosen ? "true" : null}, label + " ", h("b", {class: "count"}, number(page.counts[key])))));
  replace(top, [issuePager(page, choice, "Pages")]); replace(bottom, [issuePager(page, choice, "Pages, again")]);
  replace(region, page.issues.length ? [h("div", {class: "row head", "aria-hidden": "true"}, ["#", "ISSUE", "STATUS", "UPDATED"].map(text => h("span", {}, text))), ...page.issues.map(issue => issueRow(issue, names, location.search, now))]
   : [empty(choice.before || choice.after ? copy.pastIssues : query.size ? copy.filteredIssues : copy.emptyIssues)]);
 };
}
function issueScreen(view, id) {
 const signal = view.controller.signal, region = h("div", {class: "issue"});
 main.append(h("a", {class: "back", href: "/issues" + location.search}, "< Issues"), region);
 view.read = async () => {
  const issue = await get("/api/issues/" + encodeURIComponent(id), signal);
  if (signal.aborted) return;
  const now = Date.now(), times = [["created", issue.created_at], ["updated", issue.updated_at], ["closed", issue.closed_at]].filter(([, at]) => at);
  region.replaceChildren(h("section", {class: "issue-head"}, h("h1", {}, "#" + issue.id + " " + issue.title),
   h("p", {class: "issue-states"}, workChip(issue.state), issue.state === "open" ? workChip(issue.work_status) : null, touched(issue, now), lockLine(issue, now)),
   h("p", {class: "mono", title: issue.upstream || "global"}, issuePlace(issue) + (issue.branch ? " · " + issue.branch : "")),
   h("p", {}, (issue.kind === "ci" ? "CI" : "manual") + " · " + issue.occurrences + (issue.occurrences === 1 ? " occurrence" : " occurrences")),
   h("p", {class: "muted"}, times.flatMap(([word, at], index) => [index ? " · " : "", word + " ", h("time", {datetime: at, title: fullTime(at)}, time(at))]))),
   h("pre", {class: "issue-body"}, issue.body), importedSource(issue));
 };
}
function importedSource(issue) {
 if (!issue.imported) return document.createDocumentFragment();
 return h("section", {class: "imported-source"}, h("h3", {}, "Imported source"),
  h("p", {}, "Opened by " + (issue.author || "unknown") + " · " + fullTime(issue.created_at)),
  (issue.closer || issue.source_closed_at) ? h("p", {}, "Original closer: " + (issue.closer || "unknown") + (issue.source_closed_at ? " · " + fullTime(issue.source_closed_at) : "")) : null,
  h("h4", {}, "Source history"), h("ol", {}, (issue.history || []).map(entry => h("li", {},
   h("p", {}, entry.kind + " · " + (entry.author || "unknown") + " · " + fullTime(entry.at)),
   entry.state ? h("p", {}, entry.state) : null, entry.text ? h("pre", {}, entry.text) : null))));
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
document.addEventListener("keydown", event => {
 if (event.ctrlKey || event.metaKey || event.altKey || event.shiftKey || event.target.closest("input,textarea,select,[contenteditable]")) return;
 const paths = ["/runs", "/repos", "/issues", "/status"];
 if (/^[1-4]$/.test(event.key)) { event.preventDefault(); navigate(paths[Number(event.key) - 1]); }
 else if (event.key === "Escape") { event.preventDefault(); navigate(location.pathname.startsWith("/issues/") ? "/issues" + location.search : location.pathname.startsWith("/issues") ? "/issues" : "/runs"); }
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
