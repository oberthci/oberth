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
run(`setConn = () => {}; const runningStep = {burn:'test',step:'store',status:'running',started_at:'2026-11-05T11:59:58Z',ordinal:0};`);
assert.match(run(`stepRow(orderSteps([runningStep])[0],'')`),/data-started="2026-11-05T11:59:58Z" data-timer-kind="step"/);
const stepCell={dataset:{started:'2026-11-05T11:59:58Z',timerKind:'step'},textContent:''};
const runCell={dataset:{started:'2026-11-05T11:59:58Z'},textContent:''};
cells=[stepCell,runCell];
run('tickRunTimers()'); assert.equal(stepCell.textContent,'2s'); assert.equal(runCell.textContent,'0m 2s');
now+=1000; [...intervals.values()].find(t=>t.delay===1000).fn(); assert.equal(stepCell.textContent,'3s');
now+=62000; visibility(); assert.equal(stepCell.textContent,'1m 5s','resume recomputes from timestamp, not missed ticks');
assert.equal(streams.length+polls.length,0,'timers never request data');
assert.match(run(`stepRow(orderSteps([{...runningStep,status:'passed',finished_at:'2026-11-05T12:00:02Z'}])[0],'')`),/class="dur">4s<\/span>/);
cells=[runCell]; now+=1000;run('tickRunTimers()'); assert.equal(stepCell.textContent,'1m 5s','terminal cell leaves the ticker');
for(const step of ["{status:'pending'}","{status:'interrupted'}","{status:'running',started_at:'invalid'}"]){
  const html=run(`stepRow(orderSteps([{burn:'test',step:'store',ordinal:0,...${step}}])[0],'')`);
  assert.ok(!html.includes('data-started')&&!html.includes('NaN'));
}
assert.match(run(`stepRow(orderSteps([{...runningStep,started_at:'2099-01-01T00:00:00Z'}])[0],'')`),/&lt;1s/);
assert.ok(!run(`stepRow(orderSteps([{...runningStep,finished_at:'2026-11-05T12:00:02Z'}])[0],'')`).includes('data-started'),'finished timestamp freezes even before the status catches up');
assert.match(run(`stepRow(orderSteps([{...runningStep,finished_at:'invalid'}])[0],'')`),/data-timer-kind="step"/);
console.log('step and run clocks use timestamps without requesting logs');
