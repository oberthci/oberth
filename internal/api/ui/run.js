"use strict";
function runIdentity(run) {
 return h("p", {class: "identity", "aria-label": "Run and commit"}, h("span", {title: "Run ID"}, "run " + run.id.slice(0, 8)), " · ", h("span", {title: "Commit ID"}, "commit " + (run.commit_sha ? run.commit_sha.slice(0, 12) : "unknown")));
}
function runOrigin(run) {
 const parts = [run.trigger && run.trigger + (run.schedule_entry ? " " + run.schedule_entry : ""), run.rerun_of && "rerun of " + run.rerun_of.slice(0, 8), run.actor && "by " + run.actor, run.assist_requested && "assist requested"].filter(Boolean);
 return parts.join(" · ");
}
function runDetail(run, open) {
 return h("details", {open: open ? "" : null}, h("summary", {}, "Detail"), run.detail ? h("pre", {}, run.detail) : null,
  h("pre", {class: "mono"}, ["Run ID: " + run.id, "Object ID: " + run.object_sha, "Commit ID: " + (run.commit_sha || "unknown"), run.repo + " · " + run.ref, runOrigin(run), fullTimes(run)].filter(Boolean).join("\n")), otherRuns(run));
}
function otherRuns(run) { return h("a", {href: "/runs?" + params({repo: run.repo, ref: run.ref})}, "Other runs of this ref"); }
function runHeader(run, actions, open) {
 const counts = [run.passed + "/" + (run.steps_planned_exact ? "" : ">=") + run.step_count + " passed", run.failed && run.failed + " failed", run.pending && run.pending + " pending"].filter(Boolean).map((text, index) => h("span", {}, (index ? " · " : "") + text));
 const cancel = ["queued", "running"].includes(run.status) ? actions.querySelector("button") : null;
 const row = h("h1", {class: "run-summary", title: run.repo + " · " + run.ref}, state(run.status), clock(run), h("span", {class: "run-counts"}, counts), cancel);
 new ResizeObserver((_, observer) => { if (row.isConnected) fitRunHeader(row); else observer.disconnect(); }).observe(row);
 return [row,
  run.display_reason ? h("p", {class: "run-reason"}, run.failure ? h("b", {class: "failed-step"}, run.failure.failed_step_name || run.failure.failed_step) : null, run.display_reason) : null,
  h("div", {class: "run-meta muted"}, h("div", {class: "run-metadata"}, h("span", {class: "run-ref"}, run.repo + " · " + refName(run.ref)), runIdentity(run), runOrigin(run) ? h("span", {class: "meta-extra", "aria-label": "Started by"}, " · " + runOrigin(run)) : null,
   h("span", {class: "meta-extra"}, " · ", run.started_at ? "Started " : "Created ", h("time", {datetime: run.started_at || run.created_at, title: fullTimes(run)}, time(run.started_at || run.created_at)))), runDetail(run, open))].filter(Boolean);
}
function fitRunHeader(row) {
 const counts = row.querySelector(".run-counts"), gap = parseFloat(getComputedStyle(row).columnGap), countGap = parseFloat(getComputedStyle(counts).columnGap);
 const items = row.querySelectorAll(":scope > :not(.run-counts), .run-counts > span");
 const width = Array.from(items).reduce((total, node) => total + node.getBoundingClientRect().width, 0) + 3 * gap + (counts.children.length - 1) * countGap;
 row.classList.toggle("wrap-counts", width > row.clientWidth);
 fitRunMetadata(row.parentElement.querySelector(".run-metadata"));
}
function fitRunMetadata(region) {
 const parts = Array.from(region.children), gap = parseFloat(getComputedStyle(region).columnGap);
 region.classList.remove("only-ref");
 for (const node of parts) node.hidden = false;
 let width = parts.reduce((total, node) => total + node.getBoundingClientRect().width, 0) + (parts.length - 1) * gap;
 for (const node of [region.querySelector(".identity"), ...region.querySelectorAll(".meta-extra")].filter(Boolean)) {
  if (width <= region.clientWidth) break;
  width -= node.getBoundingClientRect().width + gap; node.hidden = true;
 }
 region.classList.toggle("only-ref", parts.filter(node => !node.hidden).length === 1);
}
