"use strict";
// Step log reading and the live log tail; they use the shared helpers of app.js, which loads after this file.
async function loadLog(view, path, args, regions, restart) {
 if (restart) { view.logs.abort(); view.logs = new AbortController(); }
 const signal = view.logs.signal;
 view.logBusy = true; view.logDirty = false; view.logReload = () => loadLog(view, path, args, regions, false);
 try {
  while (!signal.aborted) {
   try {
    const step = view.logStep;
    if (args.step !== undefined && step?.status === "queued" && !step.attempts?.length && !step.logs?.length) return;
    const native = args.node_id !== undefined;
    const result = await get(path + (native ? "/containerlog?" : "/logs?") + params({tail: true, limit: 500, ...args}), signal);
    if (signal.aborted) return;
    if (regions.output.textContent !== (result.content || "")) resetLog(regions, result.content || "");
    const open = regions.status.querySelector("details")?.open;
    regions.status.replaceChildren(...[...(result.containers || []).map(item => h("p", {}, "Container: " + item.container)), h("p", {}, result.display_reason || result.meta.reason || (result.content ? "" : copy.emptyLog[0])), detail(result.detail || result.meta.detail, open)].filter(Boolean));
    regions.controls.querySelector("button")?.remove();
    if (result.meta.has_more) regions.controls.append(button("Earlier output", () => loadLog(view, path, {...args, byte_offset: result.meta.next_byte_offset}, regions, true)));
    const target = native ? args : result.containers?.at(-1);
    const waiting = result.meta.waiting && view.runStatus === "running" && view.logStep?.status === "running" && args.byte_offset === undefined;
    if (result.meta.copying || waiting || (result.meta.follow && args.byte_offset === undefined && !target)) { await waitRetry(250, signal); continue; }
    if (!result.meta.follow || args.byte_offset !== undefined || !target) return;
    await streamLog(path, target, {regions, signal});
   } catch (error) {
    if (signal.aborted) return;
    if (!trouble(error) && !(args.step !== undefined && error.code === "not_found")) showError(error, regions.status);
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
