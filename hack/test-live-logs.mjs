// Execute the shipped dashboard functions with deterministic browser/API mocks.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import assert from 'node:assert/strict';
const source = readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8');
const sandbox = vm.createContext({ console, URLSearchParams, Map, Set, Date, document: {getElementById(){return null;}}, localStorage:{getItem(){return null;}}, setInterval() {}, clearInterval() {} });
vm.runInContext(source.slice(0, source.indexOf('/* ---------- events ---------- */')), sandbox);
const run = code => vm.runInContext(code, sandbox);
run(`
  repoName = () => 'oberth';
  const fixtureRun = { ID: 'live-run', Ref: 'fix/live', Phase: 'test', Status: 'running' };
  let polls = [];
  api = async () => { const next = polls.shift(); if (next instanceof Error) throw next; return next; };
`);
assert.equal(run(`JSON.stringify(selectedLiveLines(['[build/one] one', '[build/two] wrong', '[build/one-extra] wrong', '[build/one] repeat', '[build/one] repeat'], 'build', 'one'))`), JSON.stringify(['one', 'repeat', 'repeat']));
run(`polls.push({chunk:'[build/one] fir', offset:15}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
await run('awaitResult');
assert.equal(run('state.live.text'), '');
run(`polls.push({chunk:'st\\n[build/two] foreign\\n::oberth-log:: {"burn":"build","step":"one","state":"following"}\\n', offset:150}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
let html = await run('awaitResult');
assert.ok(html.includes('first'));
assert.ok(!html.includes('foreign'));
assert.ok(!html.includes('::oberth-log::'));
assert.ok(html.includes('following'));
assert.ok(html.includes('delivery unconfirmed'));
run(`state.live.delivery.get('build/one/').at = new Date().toISOString();`);
assert.ok(run(`liveDeliverySummary(state.live, 'build', 'one')`).includes('pod delivery: following'));
assert.ok(run(`liveDeliverySummary(state.live, 'build', 'one', Date.now()+11000)`).includes('delivery unconfirmed'));
run(`polls.push(new Error('offline')); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('reconnecting: offline'));
assert.ok(html.includes('first'));
assert.equal(run('state.live.offset'), 150);
run(`polls.push({chunk:'', offset:150}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('polling connected'));
assert.ok(!html.includes('offline'));
assert.ok(run(`liveDeliverySummary(state.live, 'build', 'one', Date.now() + 60000)`).includes('quiet period'));
run(`ingestLiveChunk(state.live, '[build/one] ' + 'x'.repeat(500000) + '\\n', Date.now());`);
assert.ok(run('state.live.text.length') <= 400000);
assert.ok(run('state.live.pending.length') <= 400000);
run(`ingestLiveChunk(state.live, ('[build/one] small\\n').repeat(10000), Date.now());`);
assert.equal(run(`(renderLogLines(splitLogText(state.live.text)).match(/class="log-line/g) || []).length`), 5001);

// Exercise the actual detail renderer's scroll restoration and combined-view
// selection, including a terminal poll that must continue draining chunks.
run(`
  state.repos = [{ID: 1, Name:'oberth'}];
  state.stepRunID = 'scroll-run'; state.step = '__summary__';
  currentRoute = () => true; setChrome = () => {}; replaceApp = () => {};
  liveLogContent = async () => 'fixture log';
  const oldTerm = {scrollHeight:1000, scrollTop:123, clientHeight:100};
  const oldSteps = {scrollTop:45};
  const newTerm = {scrollHeight:1200, scrollTop:0, clientHeight:100};
  const newSteps = {scrollTop:0};
  let termReads = 0, stepReads = 0;
  document = {getElementById(id) {
    if(id==='termBody') return termReads++ === 0 ? oldTerm : newTerm;
    if(id==='stepList') return stepReads++ === 0 ? oldSteps : newSteps;
    return null;
  }};
  setAuto = () => {};
  awaitResult = renderRunDetailView({Run:{ID:'scroll-run', RepoID:1, Status:'running', Ref:'fix/live'}, Steps:[]}, 0);
`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 123);
assert.equal(run('newSteps.scrollTop'), 45);
run(`termReads=0; stepReads=0; oldTerm.scrollTop=900; awaitResult=renderRunDetailView({Run:{ID:'scroll-run',RepoID:1,Status:'running',StartedAt:new Date().toISOString(),Ref:'fix/live'},Steps:[]},0);`);
await run('awaitResult');
assert.equal(run('newTerm.scrollTop'), 1200);

run(`
  let renderedHTML = '';
  replaceApp = html => { renderedHTML = html; };
  state.step = 'build / compile';
  state.stepRunID = 'scroll-run';
  termReads=0; stepReads=0;
  awaitResult=renderRunDetailView({Run:{ID:'scroll-run',RepoID:1,Status:'running',StartedAt:new Date().toISOString(),Ref:'fix/live'},Steps:[{Burn:'build',Step:'compile',Status:'queued',Ordinal:0}]},0);
`);
await run('awaitResult');
assert.ok(run('renderedHTML').includes('fixture log'), 'a queued pod can emit before Argo publishes Running');

console.log('live log dashboard checks passed');
