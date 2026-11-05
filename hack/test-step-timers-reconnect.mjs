// Dashboard clocks and transport recovery, using shipped functions and no network.
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
let now = Date.parse('2026-11-05T12:00:00Z');
class Clock extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
let cells = [], visibility;
const timers = new Map(), intervals = new Map();
let nextTimer = 0;
const streams = [], polls = [];
const context = vm.createContext({console, URLSearchParams, Map, Set, Date: Clock, AbortController, ReadableStream, TextDecoder,
  location:{pathname:'/runs/timers'}, localStorage:{getItem(){return 'test-token';}},
  document:{hidden:false, querySelectorAll(){return cells;}, getElementById(){return null;},
    addEventListener(name, callback){if(name === 'visibilitychange') visibility = callback;}},
  setTimeout(fn, delay){const id=++nextTimer; timers.set(id,{fn,delay});return id;}, clearTimeout(id){timers.delete(id);},
  setInterval(fn, delay){const id=++nextTimer; intervals.set(id,{fn,delay});return id;}, clearInterval(id){intervals.delete(id);},
  requestAnimationFrame(){return 1;}, cancelAnimationFrame(){},
  fetch: async (url, options) => {
    assert.equal(options.headers.Authorization, 'Bearer test-token');
    assert.ok(!url.includes('token='));
    if(url.includes('/stream?')) {
      const item = {url, options, cancelled:false};
      streams.push(item);
      const body = new ReadableStream({start(c){item.controller=c;}, cancel(){item.cancelled=true;}});
      return {ok:true, headers:new Headers({'content-type':'text/event-stream'}), body};
    }
    return new Promise((resolve,reject) => {
      polls.push({url, options, resolve:payload=>resolve({ok:true,status:200,json:async()=>payload})});
      options.signal.addEventListener('abort',()=>reject(new DOMException('aborted','AbortError')),{once:true});
    });
  }
});
vm.runInContext(source.slice(0,source.indexOf('/* ---------- events ---------- */')),context);
const run = code => vm.runInContext(code,context);
const settle = () => new Promise(resolve=>setImmediate(resolve));
const fire = async delay => {
  const entry=[...timers].find(([,t])=>t.delay===delay);
  assert.ok(entry,`missing timer ${delay}`); timers.delete(entry[0]); return entry[1].fn();
};
const frame = (chunk,offset,done=false)=>new TextEncoder().encode(`event: chunk\ndata: ${JSON.stringify({chunk,offset,done})}\n\n`);
run(`setConn = () => {}; const runningStep = {Burn:'test',Step:'store',Status:'running',StartedAt:'2026-11-05T11:59:58Z',Ordinal:0};`);
assert.match(run(`stepRow(orderSteps([runningStep])[0],'')`),/data-started="2026-11-05T11:59:58Z" data-timer-kind="step"/);
const stepCell={dataset:{started:'2026-11-05T11:59:58Z',timerKind:'step'},textContent:''};
const runCell={dataset:{started:'2026-11-05T11:59:58Z'},textContent:''};
cells=[stepCell,runCell];
run('tickRunTimers()'); assert.equal(stepCell.textContent,'2s'); assert.equal(runCell.textContent,'0m 2s');
now+=1000; [...intervals.values()].find(t=>t.delay===1000).fn(); assert.equal(stepCell.textContent,'3s');
now+=62000; visibility(); assert.equal(stepCell.textContent,'1m 5s','resume recomputes from timestamp, not missed ticks');
assert.equal(streams.length+polls.length,0,'timers never request data');
assert.match(run(`stepRow(orderSteps([{...runningStep,Status:'passed',FinishedAt:'2026-11-05T12:00:02Z'}])[0],'')`),/class="dur">4s<\/span>/);
cells=[runCell]; now+=1000;run('tickRunTimers()'); assert.equal(stepCell.textContent,'1m 5s','terminal cell leaves the ticker');
for(const step of ["{Status:'pending'}","{Status:'interrupted'}","{Status:'running',StartedAt:'invalid'}"]){
  const html=run(`stepRow(orderSteps([{Burn:'test',Step:'store',Ordinal:0,...${step}}])[0],'')`);
  assert.ok(!html.includes('data-started')&&!html.includes('NaN'));
}
assert.match(run(`stepRow(orderSteps([{...runningStep,StartedAt:'2099-01-01T00:00:00Z'}])[0],'')`),/&lt;1s/);
assert.ok(!run(`stepRow(orderSteps([{...runningStep,FinishedAt:'2026-11-05T12:00:02Z'}])[0],'')`).includes('data-started'),'finished timestamp freezes even before the status catches up');
assert.match(run(`stepRow(orderSteps([{...runningStep,FinishedAt:'invalid'}])[0],'')`),/data-timer-kind="step"/);
// Receive partial UTF-8/SSE frames, then a partial command record. No render poll.
await run(`liveLogContent({ID:'timers'},'test','store')`); await settle();
assert.equal(streams.length,1);
const bytes=frame('[test/store] first 🚀\n[test/store] par',42), split=bytes.indexOf(0xf0)+1;
streams[0].controller.enqueue(bytes.slice(0,split));await settle();assert.equal(run('state.live.offset'),-1);
streams[0].controller.enqueue(bytes.slice(split));await settle();
assert.equal(run('state.live.text'),'[test/store] first 🚀\n');assert.equal(run('state.live.pending'),'[test/store] par');
assert.equal(run('state.live.view.pending.length'),1,'chunk queued for the next animation frame before any route refresh');
streams[0].controller.error(new Error('transient'));await settle();
assert.equal(polls.length,1);assert.ok(polls[0].url.endsWith('offset=42'));
const retry=fire(1000);await settle();assert.equal(streams.length,1,'reconnect waits for in-flight poll');
polls[0].resolve({chunk:'tial\n',offset:49});await retry;await settle();
assert.equal(streams.length,2);assert.ok(streams[1].url.endsWith('offset=49'));
assert.equal(run('state.live.text'),'[test/store] first 🚀\n[test/store] partial\n');
assert.equal(run('state.live.fallback'),false);assert.equal(run('state.live.pollTimer'),null);
// Repeated failures back off even when HTTP opens successfully but no chunk arrives.
for(const delay of [2000,4000,8000,16000,30000,30000]){
  streams.at(-1).controller.error(new Error('still transient'));await settle();
  polls.at(-1).resolve({chunk:'',offset:49});await settle();await fire(delay);await settle();
}
streams.at(-1).controller.enqueue(frame('[test/store] final',68,true));await settle();
assert.equal(run('state.live.text'),'[test/store] first 🚀\n[test/store] partial\n[test/store] final\n');
assert.equal(run('state.live.done'),true);assert.equal(run('state.live.source'),null);assert.equal(timers.size,0);
assert.equal(intervals.size,1,'only local clock interval remains');
// Navigation aborts a pending fallback request and cancels its reconnect.
run('closeLiveLog()');await run(`liveLogContent({ID:'next'},'test','store')`);await settle();
streams.at(-1).controller.error(new Error('offline'));await settle();
run('const abandoned=state.live; closeLiveLog()');await settle();
assert.equal(polls.at(-1).options.signal.aborted,true);assert.equal(timers.size,0);
const count=streams.length;
run(`receiveLiveChunk(abandoned,{chunk:'stale\\n',offset:999})`);
assert.equal(run('abandoned.offset'),-1);assert.equal(streams.length,count);
// Fire the actual fallback watchdog while retry waits for cursor ownership.
await run(`liveLogContent({ID:'timeout'},'test','store')`);await settle();
streams.at(-1).controller.error(new Error('offline'));await settle();
const stalledPoll=polls.at(-1), beforeTimeout=streams.length;
const waitingRetry=fire(1000);await settle();
assert.equal(streams.length,beforeTimeout);
await fire(10000);await waitingRetry;await settle();
assert.equal(stalledPoll.options.signal.aborted,true,'10s watchdog aborts the actual fetch signal');
assert.equal(streams.length,beforeTimeout+1,'stream resumes only after aborted poll settles');
assert.ok(streams.at(-1).url.endsWith('offset=-1'),'timeout cannot advance the byte cursor');
assert.equal(run('state.live.busy'),false);assert.equal(run('state.live.pollTimer'),null);
run('closeLiveLog()');await settle();assert.equal(timers.size,0);
console.log('local step clocks, partial UTF-8, exact-cursor reconnect, bounded backoff and cancellation pass');
