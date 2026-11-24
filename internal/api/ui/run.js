"use strict";
function runMessage(run) { return [run.selected_file ? run.selected_file + " selected" : "", run.display_reason].filter(Boolean).join(" · "); }
function runIdentity(run) {
 return h("p", {class: "identity", "aria-label": "Run and commit"}, h("span", {title: "Run ID"}, "run " + run.id.slice(0, 8)), h("span", {class: "commit"}, " · ", h("span", {title: "Commit ID"}, "commit " + (run.commit_sha ? run.commit_sha.slice(0, 12) : "unknown"))));
}
function runOrigin(run) {
 const parts = [run.trigger && run.trigger + (run.schedule_entry ? " " + run.schedule_entry : ""), run.rerun_of && "rerun of " + run.rerun_of.slice(0, 8), run.actor && "by " + run.actor, run.assist_requested && "assist requested"].filter(Boolean);
 return parts.join(" · ");
}
function runDetail(run, open, origin) {
 return h("details", {open: open ? "" : null}, h("summary", {}, "Detail"), run.detail ? h("pre", {}, run.detail) : null,
  h("pre", {class: "mono"}, ["Run ID: " + run.id, "Object ID: " + run.object_sha, "Commit ID: " + (run.commit_sha || "unknown"), run.repo + " · " + run.ref, origin, fullTimes(run)].filter(Boolean).join("\n")), otherRuns(run));
}
function otherRuns(run) { return h("a", {href: "/runs?" + params({repo: run.repo, ref: run.ref})}, "Other runs of this ref"); }
function runHeader(run, actions, open) {
 const counts = [run.passed + "/" + (run.steps_planned_exact ? "" : ">=") + run.step_count + " passed", run.failed && run.failed + " failed", run.pending && run.pending + " pending"].filter(Boolean).map((text, index) => h("span", {}, (index ? " · " : "") + text));
 const origin = runOrigin(run);
 const cancel = ["queued", "running"].includes(run.status) ? actions.querySelector("button") : null;
 const row = h("div", {class: "run-summary"}, h("h1", {title: run.repo + " · " + run.ref}, state(run.status)), run.started_at ? clock(run) : null, h("span", {class: "run-counts"}, counts), cancel);
 new ResizeObserver((_, observer) => { if (row.isConnected) fitRunHeader(row); else observer.disconnect(); }).observe(row);
 return [row,
  run.selected_file ? h("p", {class: "run-file"}, "Workflow: " + run.selected_file) : null,
  run.display_reason ? h("p", {class: "run-reason"}, run.failure ? h("b", {class: "failed-step"}, run.failure.failed_step_name || run.failure.failed_step) : null, run.display_reason) : null,
  h("div", {class: "run-meta muted"}, h("div", {class: "run-metadata"}, h("span", {class: "run-ref", title: run.repo + " · " + refName(run.ref)}, run.repo + " · " + refName(run.ref)), runIdentity(run), origin ? h("span", {class: "meta-extra", "aria-label": "Started by"}, " · " + origin) : null,
   h("span", {class: "meta-extra"}, " · ", run.started_at ? "Started " : "Created ", h("time", {datetime: run.started_at || run.created_at, title: fullTimes(run)}, time(run.started_at || run.created_at)))), runDetail(run, open, origin))].filter(Boolean);
}
function fitRunHeader(row) {
 const counts = row.querySelector(".run-counts"), gap = parseFloat(getComputedStyle(row).columnGap), countGap = parseFloat(getComputedStyle(counts).columnGap);
 const items = row.querySelectorAll(":scope > :not(.run-counts), .run-counts > span");
 const width = Array.from(items).reduce((total, node) => total + node.getBoundingClientRect().width, 0) + (row.children.length - 1) * gap + (counts.children.length - 1) * countGap;
 row.classList.toggle("wrap-counts", width > row.clientWidth);
 fitRunMetadata(row.parentElement.querySelector(".run-metadata"));
}
function fitRunMetadata(region) {
 const parts = Array.from(region.children), gap = parseFloat(getComputedStyle(region).columnGap);
 for (const node of parts) node.hidden = false;
 const commit = region.querySelector(".commit"); commit.hidden = false;
 for (const node of [commit, ...region.querySelectorAll(".meta-extra")]) {
  const visible = parts.filter(part => !part.hidden);
  const width = visible.reduce((total, part) => total + part.getBoundingClientRect().width, 0) + (visible.length - 1) * gap;
  if (width <= region.clientWidth) break;
  node.hidden = true;
 }
}
function updateSelectedStep(view, selected) {
 if (!selected || JSON.stringify(selected) === JSON.stringify(view.logStep)) return;
 const open = view.logHeader?.querySelector("details")?.open;
 view.logStep = selected;
 if (view.logHeader) view.logHeader.replaceChildren(...stepHeader(selected, open));
 if (!view.logBusy) view.logReload?.();
 else view.logDirty = true;
}
function stepHeader(step, open = false) {
 return [h("h2", {}, step.name + " · " + step.status), step.display_reason && step.display_reason !== step.status ? h("p", {}, step.display_reason) : null, detail(step.detail, open)].filter(Boolean);
}
