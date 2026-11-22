"use strict";
// Issue screens; they use the shared helpers of app.js, which loads after this file.
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
 const title = "Updated " + fullTime(issue.updated_at);
 if (issue.stale) return h("span", {class: "touched stale", title}, "stale · " + span(now - Date.parse(issue.updated_at)) + " untouched");
 return h("span", {class: "touched", title}, issue.closed_at ? "closed " + span(now - Date.parse(issue.closed_at)) + " ago" : span(now - Date.parse(issue.updated_at)) + " ago");
}
function lockLine(issue, now) {
 return h("span", {class: "lock", title: issue.lock ? "Locked by " + issue.lock.owner + " until " + fullTime(issue.lock.expires_at) : null}, issue.lock ? issue.lock.owner + " · " + span(Date.parse(issue.lock.expires_at) - now, true) + " left" : "");
}
const issuePlace = issue => issue.repo || (issue.upstream && issue.upstream !== "global" ? issue.upstream : "global");
// One line per issue on the run list's baseline: number and kind, title,
// repository, status, lock and age. Only the title, a name, may be cut to
// fit its column (see app.css); it keeps its whole text as its title.
function issueRow(issue, names, back, now) {
 const place = issuePlace(issue);
 return h("a", {class: "row", href: "/issues/" + issue.id + back, "data-key": String(issue.id)},
  h("span", {class: "issue-id"}, h("b", {}, "#" + issue.id), " ", h("small", {class: issue.kind}, issue.kind === "ci" ? "CI" : "manual")),
  h("span", {class: "title", title: issue.title}, issue.title), h("span", {class: "place", title: place}, names.get(place) || place),
  workChip(issue.state === "closed" ? "closed" : issue.work_status), lockLine(issue, now), touched(issue, now));
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
  replace(region, page.issues.length ? [h("div", {class: "row head", "aria-hidden": "true"}, ["#", "ISSUE", "REPOSITORY", "STATUS", "LOCK", "UPDATED"].map(text => h("span", {}, text))), ...page.issues.map(issue => issueRow(issue, names, location.search, now))]
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
   h("p", {class: "issue-states"}, workChip(issue.state), issue.state === "open" ? workChip(issue.work_status) : null, touched(issue, now), issue.lock ? ["locked by", lockLine(issue, now)] : null),
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
