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
// Scope raw records before interpreting carriage returns as visual lines.
// A sibling's progress text must not impersonate a selected-step prefix.
run(`polls.push({chunk:'[build/one] progress-1\\rprogress-2\\n[build/two] sibling\\r[build/one] foreign-progress\\n', offset:250}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('progress-2'), 'carriage-return output stays visible');
assert.ok(!html.includes('foreign-progress'), 'carriage returns cannot change attribution');
assert.ok(!html.includes('[build/one]'), 'selected logs omit the retained attribution prefix');
run(`polls.push({chunk:'[build/one] final-without-newline', offset:280, done:true}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
html = await run('awaitResult');
assert.ok(html.includes('final-without-newline'), 'the final poll flushes an unterminated tail');
assert.equal(run('state.live.pending'), '');
assert.equal(run('state.live.done'), true);
// Empty final polls must flush a fragment retained by the preceding poll too.
run(`state.live.done = false; polls.push({chunk:'[build/one] split-final', offset:300}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
await run('awaitResult');
assert.ok(!run('state.live.text').includes('split-final'));
run(`polls.push({chunk:'', offset:300, done:true}); awaitResult = liveLogContent(fixtureRun, 'build', 'one');`);
assert.ok((await run('awaitResult')).includes('split-final'));
// Retained single-step logs remove only the outer transport attribution,
// leaving command-authored bracketed messages untouched.
run(`polls.push({output:'[release-build/full-our-step-name] actual log\\n[release-build/full-our-step-name] [INFO] user message\\n'}); awaitResult = stepLogContent('completed', {Burn:'release-build', Step:'full-our-step-name', Status:'passed'}, false);`);
html = await run('awaitResult');
assert.ok(html.includes('actual log') && html.includes('[INFO] user message'));
assert.ok(!html.includes('[release-build/full-our-step-name]'));
html = run(`renderLogLines(['[build/one] actual one', '[build/two] actual two', '[build/one] actual next'], {groupSteps:true})`);
assert.ok(!html.includes('[build/one]') && !html.includes('[build/two]'));
assert.equal((html.match(/log-hdr/g) || []).length, 3, 'combined output labels each change of step');
html = run(`renderLogLines(splitAttributedLogText('[build/two] progress\\r[build/one] command text\\n'), {groupSteps:true})`);
assert.equal((html.match(/log-hdr/g) || []).length, 1, 'CR updates cannot forge combined-view step headers');
assert.ok(html.includes('── build / two ──'));

// API/plan order need not be execution order. Equal-time parallel steps use a
// stable ordinal tie-breaker; pending steps remain after all started steps.
run(`const orderedFixture = [
  {Burn:'ci', Step:'pending', Ordinal:0, Status:'pending'},
  {Burn:'ci', Step:'late', Ordinal:1, StartedAt:'2026-10-23T01:00:02Z'},
  {Burn:'ci', Step:'parallel-b', Ordinal:3, StartedAt:'2026-10-23T01:00:01Z'},
  {Burn:'ci', Step:'early', Ordinal:99, StartedAt:'2026-10-23T01:00:00Z'},
  {Burn:'ci', Step:'parallel-a', Ordinal:2, StartedAt:'2026-10-23T01:00:01Z'}
];`);
assert.equal(run(`orderSteps(orderedFixture).map(row => row.step.Step).join(',')`), 'early,parallel-a,parallel-b,late,pending');
run(`const legacyRetryFixture = [
  {Burn:'build(2)', Step:'compile', Ordinal:3, StartedAt:'2026-10-23T01:00:03Z'},
  {Burn:'lint', Step:'lint', Ordinal:1, StartedAt:'2026-10-23T01:00:01Z'},
  {Burn:'build', Step:'compile', Ordinal:0, StartedAt:'2026-10-23T01:00:00Z'},
  {Burn:'build(1)', Step:'compile', Ordinal:2, StartedAt:'2026-10-23T01:00:02Z'}
];`);
assert.equal(run(`orderSteps(legacyRetryFixture).map(row => row.step.Burn).join(',')`), 'build,lint,build(1),build(2)');
// An oversized partial record keeps its original attribution. Trimming its
// head would make its continuation look like a fresh record from another step.
run(`state.live.text = ''; ingestLiveChunk(state.live, '[build/two] ' + 'x'.repeat(200000-12) + '[build/one] forged' + 'x'.repeat(300000-18), Date.now());`);
run(`ingestLiveChunk(state.live, ' remainder\\n[build/one] real\\n', Date.now());`);
assert.equal(run(`selectedLiveLines(state.live.text.split('\\n'), 'build', 'one').join('\\n')`), 'real');
assert.ok(run('state.live.text').includes('[truncated]'));
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
